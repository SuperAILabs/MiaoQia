// Package testenv 为内核与子系统的集成测试准备真实的 PostgreSQL 与 Kafka（研发规范：集成测试使用真实组件）。
// 需要环境变量 MIAOQIA_TEST_PG（超级用户连接串）与 MIAOQIA_TEST_KAFKA（broker 地址）；未设置时跳过测试。
// 本地由 deploy/local/up.sh 启动组件。
package testenv

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Database 是一个临时的子系统数据库：owner 执行迁移，app 是受 RLS 约束的应用账号，relay 是中继账号。
type Database struct {
	Name                       string
	OwnerDSN, AppDSN, RelayDSN string
}

func pgURL(t testing.TB) string {
	u := os.Getenv("MIAOQIA_TEST_PG")
	if u == "" {
		t.Skip("未设置 MIAOQIA_TEST_PG（例如 postgres://postgres:postgres@localhost:5432/postgres）")
	}
	return u
}

// NewDatabase 创建临时数据库与三个账号，测试结束时删除。
func NewDatabase(t testing.TB) Database {
	t.Helper()
	admin := pgURL(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	name := fmt.Sprintf("kt%d_%04d", time.Now().Unix()%100000, rand.IntN(10000))
	stmts := []string{
		fmt.Sprintf(`CREATE ROLE %s_owner LOGIN PASSWORD 'pw'`, name),
		fmt.Sprintf(`CREATE ROLE %s_app LOGIN PASSWORD 'pw' NOBYPASSRLS`, name),
		fmt.Sprintf(`CREATE ROLE %s_relay LOGIN PASSWORD 'pw'`, name),
		fmt.Sprintf(`CREATE DATABASE %s OWNER %s_owner`, name, name),
	}
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	dsn := func(user string) string {
		u, _ := url.Parse(admin)
		u.User = url.UserPassword(user, "pw")
		u.Path = "/" + name
		return u.String()
	}
	oc, err := pgx.Connect(ctx, dsn(name+"_owner"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s_app`, name),
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s_app`, name),
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO %s_app`, name),
		// 测试库中的中继账号名不是 outbox_relay，单独授予
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s_relay`, name),
	} {
		if _, err := oc.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	oc.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
		for _, r := range []string{"owner", "app", "relay"} {
			_, _ = c.Exec(context.Background(), fmt.Sprintf(`DROP ROLE IF EXISTS %s_%s`, name, r))
		}
	})
	return Database{Name: name, OwnerDSN: dsn(name + "_owner"), AppDSN: dsn(name + "_app"), RelayDSN: dsn(name + "_relay")}
}

// GrantRelay 在内核表创建后授予测试中继账号发件箱权限。
func (d Database) GrantRelay(t testing.TB) {
	t.Helper()
	c, err := pgx.Connect(context.Background(), d.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	if _, err := c.Exec(context.Background(), fmt.Sprintf(`GRANT SELECT, UPDATE (published_at) ON kernel_outbox TO %s_relay`, d.Name)); err != nil {
		t.Fatal(err)
	}
}

// Brokers 返回 Kafka broker 地址；未设置时跳过测试。
func Brokers(t testing.TB) []string {
	b := os.Getenv("MIAOQIA_TEST_KAFKA")
	if b == "" {
		t.Skip("未设置 MIAOQIA_TEST_KAFKA（例如 localhost:9092）")
	}
	return []string{b}
}

// Topics 创建带随机后缀的临时主题，返回 名称→实际主题。
func Topics(t testing.TB, names ...string) map[string]string {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(Brokers(t)...))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	suffix := fmt.Sprintf("%d%04d", time.Now().Unix()%100000, rand.IntN(10000))
	out := map[string]string{}
	var all []string
	for _, n := range names {
		out[n] = n + "-t" + suffix
		all = append(all, out[n])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := adm.CreateTopics(ctx, 3, 1, nil, all...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := kgo.NewClient(kgo.SeedBrokers(Brokers(t)...))
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = kadm.NewClient(c).DeleteTopics(context.Background(), all...)
	})
	return out
}
