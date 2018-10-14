// Copyright 2018 Benjamin Estes. All rights reserved.  Use of this
// source code is governed by an MIT-style license that can be found
// in the LICENSE file.

package crawler

import (
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/benjaminestes/crawl/crawler/data"
	"github.com/benjaminestes/robots"
)

type Crawler struct {
	depth   int
	queue   []*data.Address
	seen    map[string]bool // key = full text of URL
	results chan *data.Result

	// robots maintains a robots.txt matcher for every encountered
	// domain
	robots map[string]func(string) bool

	// mu guards nextqueue when multiple fetches may try to write
	// to it simultaneously
	nextqueue []*data.Address
	mu        sync.Mutex

	// wg waits for all spawned fetches to complete before
	// crawling the next level
	wg sync.WaitGroup

	// connections is a semaphore ensuring no more than
	// Config.Connections connections are active
	connections chan bool

	// wait is the parsed version of Config.WaitTime
	wait            time.Duration
	lastRequestTime time.Time

	// (in|ex)clude are the compiled versions of
	// Config.(In|Ex)clude, which are []string.
	include []*regexp.Regexp
	exclude []*regexp.Regexp

	client *http.Client
	*Config
}

// Crawl creates and starts a Crawler, and returns a pointer to it.
// The Crawler is a state machine running in its own
// goroutine. Therefore, calling this function may initiate many
// network requests, even before any results are requested from it.
func Crawl(config *Config) *Crawler {
	// This should anticipate a failure condition
	first := data.MakeAddressFromString(config.Start)
	return CrawlList(config, []*data.Address{first})
}

// initializeClient uses a config object to create an http.Client
// that conforms to the end-user's requirements.
func initializedClient(config *Config) *http.Client {
	return &http.Client{
		// Because we're checking the behavior of specific
		// URLs to understand whether they behave as expected,
		// we do not want to follow redirects.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			MaxIdleConns: config.Connections,
			// FIXME: make configurable
			IdleConnTimeout: 30 * time.Second,
		},
	}
}

// CrawlList starts and returns a *Crawler that is working from an
// input slice of Addresses rather than the start URL specified in
// config.
func CrawlList(config *Config, q []*data.Address) *Crawler {
	// FIXME: Should handle error
	wait, _ := time.ParseDuration(config.WaitTime)

	c := &Crawler{
		client:      initializedClient(config),
		connections: make(chan bool, config.Connections),
		seen:        make(map[string]bool),
		results:     make(chan *data.Result, config.Connections),
		queue:       q,
		Config:      config,
		wait:        wait,
		robots:      make(map[string]func(string) bool),
		include:     preparePattern(config.Include),
		exclude:     preparePattern(config.Exclude),
	}

	// If a URL has not been seen when the crawler processes a
	// link, that URL will be added to the next queue to crawl. It
	// does not impact whether a URL in the current queue will be
	// crawled. Therefore, we add all URLs from the initial queue
	// to the set of URLs that have been seen, before the crawl
	// starts.
	for _, addr := range c.queue {
		c.seen[addr.Full] = true
	}

	c.start()

	return c
}

// start begins the state machine for the crawler in its own goroutine
// and returns control to the calling function.
func (c *Crawler) start() {
	go func() {
		for f := crawlStartQueue; f != nil; f = f(c) {
		}
		close(c.results)
	}()
}

// preparePattern takes a []string of regexp patterns and compiles them.
func preparePattern(patterns []string) (compiled []*regexp.Regexp) {
	for _, s := range patterns {
		r := regexp.MustCompile(s)
		compiled = append(compiled, r)
	}
	return
}

// willCrawl says that a string representing a URL will or will not be
// included in a crawl based on the include and exclude fields of the
// Crawler object.
func (c *Crawler) willCrawl(fullurl string) bool {
	// 1. If a URL matches any exclude rule, it will not be
	// crawled.
	for _, r := range c.exclude {
		if r.MatchString(fullurl) {
			return false
		}
	}

	// 2. If a URL matches any include rule, it will be crawled.
	for _, r := range c.include {
		if r.MatchString(fullurl) {
			return true
		}
	}

	// 3. If a URL matches neither an exclude nor include rule,
	// then the presence or absence of any include rules
	// determines whether it will be crawled. If there are no
	// include rules, then the URL will still be crawled.
	if len(c.include) > 0 {
		return false
	}
	return true
}

// addRobots creates a robots.txt matcher from a URL string. If there
// is a problem reading from robots.txt, treat it as a server error.
func (c *Crawler) addRobots(fullurl string) {
	rtxtURL, err := robots.Locate(fullurl)
	if err != nil {
		// Error parsing fullurl.
		return
	}

	resp, err := http.Get(rtxtURL)
	if err != nil {
		rtxt, _ := robots.From(503, nil)
		c.robots[rtxtURL] = rtxt.Tester(c.RobotsUserAgent)
		return
	}
	defer resp.Body.Close()

	rtxt, err := robots.From(resp.StatusCode, resp.Body)
	if err != nil {
		rtxt, _ := robots.From(503, nil)
		c.robots[rtxtURL] = rtxt.Tester(c.RobotsUserAgent)
		return
	}

	c.robots[rtxtURL] = rtxt.Tester(c.RobotsUserAgent)
}

// Returns the next result from the crawl. Results are guaranteed to come
// out in order ascending by depth. Within a "level" of depth, there is
// no guarantee as to which URLs will be crawled first.
//
// Result objects are suitable for Marshling into JSON format and conform
// to the schema exported by the crawler.Schema package.
func (c *Crawler) Next() *data.Result {
	node, ok := <-c.results
	if !ok {
		return nil
	}
	return node
}

// resetWait sets the last time the crawler spawned a request.
func (c *Crawler) resetWait() {
	c.lastRequestTime = time.Now()
}

// merge takes a []*data.Link and adds it to the next queue to be
// crawled.  In other words, it assembles the URLs that represent the
// next level of the crawl. Many merges could be simultaneously
// active.
func (c *Crawler) merge(links []*data.Link) {
	// This is how the crawler terminates — it will encounter an
	// empty queue if no URLs have been added to the next queue.
	if !(c.depth < c.MaxDepth) {
		return
	}
	for _, link := range links {
		if link.Address == nil || !c.willCrawl(link.Address.Full) {
			continue
		}

		// This is the only place that c.seen is inspected or
		// mutated after it is initialized.
		c.mu.Lock()
		if _, ok := c.seen[link.Address.Full]; !ok {
			if !(link.Nofollow && c.RespectNofollow) {
				c.seen[link.Address.Full] = true
				c.nextqueue = append(c.nextqueue, link.Address)
			}
		}
		c.mu.Unlock()
	}
}

// fetch requests a URL, hydrates a result object based on its
// contents, if any, and initiates a merge of the links discovered in
// the process.
func (c *Crawler) fetch(addr *data.Address) {
	result := data.MakeResult(addr, c.depth)

	req, err := http.NewRequest("GET", addr.Full, nil)
	if err != nil {
		return
	}

	req.Header.Set("User-Agent", c.UserAgent)

	resp, err := c.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	result.Hydrate(resp)
	links := result.Links

	// If the result doesn't redirect, we say it resolves to itself.
	result.ResolvesTo = result.Address

	// If redirect, the single link is the target of the
	// redirect. We update ResolvesTo.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		result.ResolvesTo = data.MakeAddressFromRelative(addr, resp.Header.Get("Location"))
		links = []*data.Link{data.MakeLink(addr, resp.Header.Get("Location"), "", false)}
	}

	c.merge(links)
	c.results <- result
}