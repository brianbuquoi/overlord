package store_test

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/orcastrator/orcastrator/internal/store"
	redisstore "github.com/orcastrator/orcastrator/internal/store/redis"
)

func TestRedisStoreConformance(t *testing.T) {
	t.Parallel()

	RunConformanceTests(t, func() store.Store {
		mr := miniredis.RunT(t)
		client := goredis.NewClient(&goredis.Options{
			Addr: mr.Addr(),
		})
		t.Cleanup(func() { client.Close() })
		return redisstore.New(client, "test:", 1*time.Hour)
	})
}
