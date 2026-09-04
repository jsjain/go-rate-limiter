package rate_limiter

import (
	"github.com/redis/rueidis"
)

// Copyright (c) 2017 Pavel Pravosud
// https://github.com/rwz/redis-gcra/blob/master/vendor/perform_gcra_ratelimit.lua
//
// Both scripts work in whole microseconds rather than floating-point seconds.
// Redis TIME has microsecond resolution, and a microsecond count measured from
// the 2017 epoch stays well below 2^53, so every value here is an exact integer
// in Lua's doubles and the arithmetic is exact.
//
// The original computed `now - ((tat + increment) - burst_offset)` in seconds,
// subtracting two numbers of epoch magnitude. At ~2.9e8 seconds a float64's
// spacing is ~6e-8, and that cancellation lost enough precision to turn a
// remaining count of 9 into 8.999999976, which Redis truncated to 8 on the way
// back: PerSecond(10) reported 9 available tokens as 8.
//
// The emission interval and burst offset arrive precomputed from Go, so the
// script derives nothing per call, and every value it returns is an integer
// count of microseconds rather than a formatted float.
const luaPreamble = `
-- this script has side-effects, so it requires replicate commands mode
redis.replicate_commands()
local rate_limit_key = KEYS[1]
local emission_interval = tonumber(ARGV[1]) -- microseconds per event
local burst_offset = tonumber(ARGV[2])      -- emission_interval * burst
local cost = tonumber(ARGV[3])

-- Redis returns time as two integers: epoch seconds and microseconds. Measuring
-- from Jan 1 2017 keeps the microsecond count near 2.9e14, far below the 2^53
-- limit for exact integers in a double. That holds past the year 2200.
local t = redis.call("TIME")
local now = (t[1] - 1483228800) * 1000000 + tonumber(t[2])

local tat = redis.call("GET", rate_limit_key)
if not tat then
  tat = now
else
  tat = tonumber(tat)
end
if tat < now then
  tat = now
end
`

// Returned values are microseconds; retry_after is -1 when the request was
// allowed.
var allowN = rueidis.NewLuaScript(luaPreamble + `
local new_tat = tat + emission_interval * cost
local diff = now - (new_tat - burst_offset)
if diff < 0 then
  return {0, 0, -diff, tat - now}
end
local reset_after = new_tat - now
if reset_after > 0 then
  redis.call("SET", rate_limit_key, new_tat, "EX", math.ceil(reset_after / 1000000))
end
return {cost, math.floor(diff / emission_interval), -1, reset_after}
`)

var allowAtMost = rueidis.NewLuaScript(luaPreamble + `
local diff = now - (tat - burst_offset)
local remaining = math.floor(diff / emission_interval)
if remaining < 1 then
  return {0, 0, emission_interval - diff, tat - now}
end
if remaining < cost then
  -- diff and emission_interval are exact integers, so the quotient is
  -- integer-valued and this assignment charges exactly what is reported.
  cost = remaining
  remaining = 0
else
  remaining = remaining - cost
end
local new_tat = tat + emission_interval * cost
local reset_after = new_tat - now
if reset_after > 0 then
  redis.call("SET", rate_limit_key, new_tat, "EX", math.ceil(reset_after / 1000000))
end
return {cost, remaining, -1, reset_after}
`)
