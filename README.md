# Rate limiting for implementation for rueidis package

[![Go Reference](https://pkg.go.dev/badge/github.com/rueian/rueidis.svg)](https://pkg.go.dev/github.com/jsjain/go-rate-limiter)

This package is based on [rwz/redis-gcra](https://github.com/rwz/redis-gcra) and implements
[GCRA](https://en.wikipedia.org/wiki/Generic_cell_rate_algorithm) (aka leaky bucket) and [go-redis redis_rate](https://github.com/go-redis/redis_rate) for go code inspiration for rate
limiting based on Redis. The code requires Redis version 3.2 or newer since it relies on
[replicate_commands](https://redis.io/commands/eval#replicating-commands-instead-of-scripts)
feature.

## Installation

redis_rate supports 2 last Go versions and requires a Go version with
[modules](https://github.com/golang/go/wiki/Modules) support. So make sure to initialize a Go
module:

```shell
go mod init github.com/my/repo
```

```shell
go get github.com/jsjain/go-rate-limiter
```

## Example

```go
package main

import (
	"context"
	"fmt"

	rl "github.com/jsjain/go-rate-limiter"
)

func ExampleNewLimiter() {
	client, err := rueidis.NewClient(rueidis.ClientOption{
	InitAddress:           []string{"127.0.0.1:6379"},
  })
  if err != nil {
    panic(err)
  }
	limiter := rl.NewLimiter(client)
	res, err := limiter.Allow(ctx, "key")
	if err != nil {
		panic(err)
	}
	fmt.Println("allowed", res.Allowed, "remaining", res.Remaining)
	// Output: allowed 1 remaining 9
}
```

### Setting custom rate limits for different keys and default rate limit

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/alphadose/haxmap"
	rl "github.com/jsjain/go-rate-limiter"
	"github.com/redis/rueidis"
)

func NewLimiterWithCustomLimits() {
	client, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress: []string{"127.0.0.1:6379"},
	})
	if err != nil {
		panic(err)
	}
	customLimits := haxmap.New[string, Limit]()
	customLimits.Set("key1", Limit{Burst: 50, Rate: 50, Period: time.Second})
	limiter := rl.NewLimiter(client, WithCustomLimits(customLimits), WithRateLimit(rl.PerSecond(20)))
	
	res, err := limiter.Allow(context.Background(), "key")
	if err != nil {
		panic(err)
	}
	fmt.Println("allowed", res.Allowed, "remaining", res.Remaining)
	// Output: allowed 1 remaining 19

	res, err := limiter.Allow(context.Background(), "key1")
	if err != nil {
		panic(err)
	}
	fmt.Println("allowed", res.Allowed, "remaining", res.Remaining)
	// Output: allowed 1 remaining 49
}

```
