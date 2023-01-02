# Rate limiting for implementation for rueidis package

[![Build status](https://badge.buildkite.com/d15fbd91b3b22b55c8d799564f84918a322118ae02590858c4.svg)](https://buildkite.com/rueian/rueidis)
[![Go Reference](https://pkg.go.dev/badge/github.com/rueian/rueidis.svg)](https://pkg.go.dev/github.com/rueian/rueidis)

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

	"github.com/jsjain/go-rate-limiter"
)

func ExampleNewLimiter() {
	client, err := rueidis.NewClient(rueidis.ClientOption{
	InitAddress:           []string{"127.0.0.1:6379"},
  })
  if err != nil {
    panic(err)
  }
	limiter := redis_rate.NewLimiter(client)
	res, err := limiter.Allow(ctx, "key", redis_rate.PerSecond(10))
	if err != nil {
		panic(err)
	}
	fmt.Println("allowed", res.Allowed, "remaining", res.Remaining)
	// Output: allowed 1 remaining 9
}
```
