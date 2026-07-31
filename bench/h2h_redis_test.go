package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/redis/go-redis/v9"
)

// benchRedis is the out-of-process point of reference: localhost TCP,
// persistence off (--save ” --appendonly no), so every number is
// dominated by round trips, not storage. The mapping is the honest
// Redis-native one, not a transaction emulation:
//
//   - Insert: INCR for the id, then SET — two round trips, the
//     standard counter-keyed insert pattern.
//   - Heavy: one MULTI/EXEC pipeline of 400 GETs + INCR + SET — a
//     single round trip, atomic on the server, but NOT an isolated
//     read-then-write transaction: reads cannot inform the write.
//     Anything needing that would add WATCH or a Lua script.
//
// Skips if no server is listening on the bench port.
func benchRedis(b *testing.B, heavy bool) {
	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:63799", PoolSize: 16})
	defer client.Close()
	if err := client.Ping(ctx).Err(); err != nil {
		b.Skipf("no redis-server on 127.0.0.1:63799: %v", err)
	}
	if err := client.FlushDB(ctx).Err(); err != nil {
		b.Fatal(err)
	}

	if _, err := client.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i := 1; i <= seedRows; i++ {
			p.Set(ctx, fmt.Sprintf("ev:%d", i), "seed", 0)
		}
		p.Set(ctx, "ev:id", seedRows, 0)
		return nil
	}); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if !heavy {
				id, err := client.Incr(ctx, "ev:id").Result()
				if err != nil {
					b.Error(err)
					return
				}
				if err := client.Set(ctx, fmt.Sprintf("ev:%d", id), "bench", 0).Err(); err != nil {
					b.Error(err)
					return
				}
				continue
			}
			// Key for the write drawn inside the atomic block via a
			// second INCR result is impossible in a pipeline; draw it
			// first (one extra round trip), then batch reads + write.
			id, err := client.Incr(ctx, "ev:id").Result()
			if err != nil {
				b.Error(err)
				return
			}
			if _, err := client.TxPipelined(ctx, func(p redis.Pipeliner) error {
				for i := 1; i <= readsPer; i++ {
					p.Get(ctx, fmt.Sprintf("ev:%d", i))
				}
				p.Set(ctx, fmt.Sprintf("ev:%d", id), "bench", 0)
				return nil
			}); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkInsert_redis(b *testing.B) { benchRedis(b, false) }
func BenchmarkHeavy_redis(b *testing.B)  { benchRedis(b, true) }
