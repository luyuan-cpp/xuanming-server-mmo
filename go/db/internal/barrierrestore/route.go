package barrierrestore

import (
	"github.com/redis/go-redis/v9"
)

func validSchema(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i, c := range s {
		if c != '_' && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func (r Restorer) mapping() *redis.Client {
	if r.MappingRedis != nil {
		return r.MappingRedis
	}
	return r.Redis
}
