package main

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
)

// requestCounters tallies the outbound requests made during one check. It is
// carried on the context rather than stored on the clients, so the program and
// report schedulers can run concurrently through the same H1Client and Discord
// client without either one counting the other's requests.
type requestCounters struct {
	general        atomic.Int64
	report         atomic.Int64
	scope          atomic.Int64
	hackerOneRetry atomic.Int64
	discord        atomic.Int64
	discordRetry   atomic.Int64
}

type requestCountersKey struct{}

func withRequestCounters(ctx context.Context) (context.Context, *requestCounters) {
	counters := &requestCounters{}
	return context.WithValue(ctx, requestCountersKey{}, counters), counters
}

// countersFrom returns nil when no check is counting, which keeps the clients
// usable from tests and from any future caller that does not want a tally.
func countersFrom(ctx context.Context) *requestCounters {
	counters, _ := ctx.Value(requestCountersKey{}).(*requestCounters)
	return counters
}

func (c *requestCounters) countHackerOne(class requestClass, attempt int) {
	if c == nil {
		return
	}
	switch class {
	case reportRead:
		c.report.Add(1)
	case scopeRead:
		c.scope.Add(1)
	default:
		c.general.Add(1)
	}
	if attempt > 0 {
		c.hackerOneRetry.Add(1)
	}
}

func (c *requestCounters) countDiscord(attempt int) {
	if c == nil {
		return
	}
	c.discord.Add(1)
	if attempt > 0 {
		c.discordRetry.Add(1)
	}
}

// HackerOneTotal and DiscordTotal are reported separately rather than as one
// figure: the two APIs have unrelated rate limits, so a combined count hides
// which side a check actually spent its budget on.
func (c *requestCounters) HackerOneTotal() int64 {
	if c == nil {
		return 0
	}
	return c.general.Load() + c.report.Load() + c.scope.Load()
}

func (c *requestCounters) DiscordTotal() int64 {
	if c == nil {
		return 0
	}
	return c.discord.Load()
}

// Summary renders the tally for the end-of-interval log line.
func (c *requestCounters) Summary() string {
	if c == nil {
		return "HackerOne API: 0 requests | Discord: 0 requests"
	}
	hackerOne := fmt.Sprintf("HackerOne API: %d requests (%d list/detail, %d report, %d scope",
		c.HackerOneTotal(), c.general.Load(), c.report.Load(), c.scope.Load())
	if retried := c.hackerOneRetry.Load(); retried > 0 {
		hackerOne += ", " + strconv.FormatInt(retried, 10) + " retried"
	}
	hackerOne += ")"

	discord := "Discord: " + strconv.FormatInt(c.DiscordTotal(), 10) + " requests"
	if retried := c.discordRetry.Load(); retried > 0 {
		discord += " (" + strconv.FormatInt(retried, 10) + " retried)"
	}
	return hackerOne + " | " + discord
}
