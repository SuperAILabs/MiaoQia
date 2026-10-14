// Package ids 生成类型化 ULID 标识（前缀登记在 contracts/identifiers.yaml）。
package ids

import (
	"crypto/rand"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// New 返回“<前缀>_<ULID>”。ULID 按时间单调，便于索引局部性。
func New(prefix string) string {
	return prefix + "_" + ulid.MustNew(ulid.Timestamp(time.Now()), ulid.Monotonic(rand.Reader, 0)).String()
}

// Prefix 返回标识的前缀。
func Prefix(id string) string {
	p, _, _ := strings.Cut(id, "_")
	return p
}
