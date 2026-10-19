// platform 是 platform 部署单元（Go）的入口：同一镜像按进程角色启动，加载部署清单中该角色的子系统
// （docs/03-architecture/07-deployment-topology.md 第 2.3 节）。
//
//	platform -role api|consumer|outbox-relay|scheduler|workflow-worker
//	platform migrate                         执行本单元全部子系统的数据库迁移
//	platform grant <subject> <relation> <object>   写入一条授权关系（初始化与运维）
//
// 配置（环境变量）：
//
//	MIAOQIA_DEPLOY_DIR            部署清单目录（含 subsystems.yaml、platform/roles.yaml），默认 deploy
//	MIAOQIA_KAFKA_BROKERS         Kafka broker 地址，逗号分隔
//	MIAOQIA_<子系统>_DB_DSN        子系统数据库（应用账号）；MIAOQIA_<子系统>_DB_OWNER_DSN 用于迁移
//	MIAOQIA_<子系统>_<KEY>         子系统配置，例如 MIAOQIA_IDENTITY_OPENFGA_URL
//	MIAOQIA_BINDING_<子系统>       local（默认）或 remote；remote 时经 MIAOQIA_<子系统>_URL 调用远程接口
//	MIAOQIA_HTTP_ADDR / MIAOQIA_INTERNAL_ADDR   对外与内部监听地址，默认 :8090 / :8091
//	MIAOQIA_GATEWAY_ISSUER / MIAOQIA_GATEWAY_AUDIENCE   HTTPS Keycloak realm与资源受众
//	MIAOQIA_ENV                 默认生产策略；仅development允许实验环境明文
//	MIAOQIA_PUBLIC_TLS_CERT / KEY  对外HTTPS证书文件
//	MIAOQIA_INTERNAL_TLS_CERT / KEY / CA  内部mTLS证书、私钥及CA
//	MIAOQIA_INTERNAL_ALLOWED_PEERS  精确SPIFFE URI SAN白名单
//	MIAOQIA_RELAY_DSN_TEMPLATE    中继连接串模板，{db} 替换为数据库代码
//	OTEL_EXPORTER_OTLP_ENDPOINT   追踪导出地址（OTLP/gRPC）
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.yaml.in/yaml/v3"

	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/relay"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/kernel/tracing"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
	"github.com/superailabs/miaoqia/services/subsystems/aiplatform"
	"github.com/superailabs/miaoqia/services/subsystems/assessment"
	"github.com/superailabs/miaoqia/services/subsystems/channels"
	"github.com/superailabs/miaoqia/services/subsystems/commerce"
	"github.com/superailabs/miaoqia/services/subsystems/customercontext"
	"github.com/superailabs/miaoqia/services/subsystems/detection"
	"github.com/superailabs/miaoqia/services/subsystems/gateway"
	"github.com/superailabs/miaoqia/services/subsystems/governance"
	"github.com/superailabs/miaoqia/services/subsystems/identity"
	identityapi "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	identityremote "github.com/superailabs/miaoqia/services/subsystems/identity/api/remote"
	"github.com/superailabs/miaoqia/services/subsystems/learning"
	"github.com/superailabs/miaoqia/services/subsystems/operations"
	"github.com/superailabs/miaoqia/services/subsystems/pursuit"
	"github.com/superailabs/miaoqia/services/subsystems/research"
	"github.com/superailabs/miaoqia/services/subsystems/search"
)

// modules 是本单元全部子系统；顺序即启动顺序（提供绑定的子系统在前）。
var modules = []func() runtime.Module{
	identity.Module, governance.Module, commerce.Module, aiplatform.Module, gateway.Module,
	customercontext.Module, detection.Module, research.Module, assessment.Module, pursuit.Module,
	learning.Module, channels.Module, search.Module, operations.Module,
}

// remotes 是可切换为远程绑定的接口：子系统 → 绑定名与远程客户端构造。
var remotes = map[string]struct {
	binding string
	client  func(url string) (any, error)
}{
	identityapi.Name: {identityapi.AuthorizerBinding, func(u string) (any, error) { return identityremote.NewAuthorizer(u) }},
}

func envKey(sub, key string) string {
	return "MIAOQIA_" + strings.ToUpper(strings.ReplaceAll(sub, "-", "_")) + "_" + key
}

type manifest struct {
	Roles map[string]runtime.RoleSpec `yaml:"roles"`
}

type registry struct {
	Subsystems map[string]struct {
		Language   string   `yaml:"language"`
		Unit       string   `yaml:"unit"`
		Database   string   `yaml:"database"`
		DataScopes []string `yaml:"data_scopes"`
	} `yaml:"subsystems"`
}

func readYAML(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, out)
}

func main() {
	role := flag.String("role", "", "进程角色")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch {
	case flag.Arg(0) == "migrate":
		if flag.NArg() != 1 {
			err = fmt.Errorf("迁移不接受额外位置参数；请使用 -role <角色> migrate")
		} else {
			err = migrate(ctx, log, *role)
		}
	case flag.Arg(0) == "grant" && flag.NArg() == 4:
		err = identity.Grant(ctx, os.Getenv(envKey("identity", "OPENFGA_URL")), flag.Arg(1), flag.Arg(2), flag.Arg(3))
	case *role != "":
		err = run(ctx, log, *role)
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("退出", "error", err)
		os.Exit(1)
	}
}

func deployDir() string {
	if d := os.Getenv("MIAOQIA_DEPLOY_DIR"); d != "" {
		return d
	}
	return "deploy"
}

func migrate(ctx context.Context, log *slog.Logger, role string) error {
	if role == "" {
		role = "api"
	}
	spec, scopes, _, err := loadRole(role)
	if err != nil {
		return err
	}
	for _, mk := range modules {
		m := mk()
		if _, selected := scopes[m.Name]; !selected || m.Database == "" {
			continue
		}
		dsn := os.Getenv(envKey(m.Name, "DB_OWNER_DSN"))
		if dsn == "" {
			return fmt.Errorf("未配置 %s", envKey(m.Name, "DB_OWNER_DSN"))
		}
		if err := db.Migrate(ctx, dsn, m.Database+"_app", m.Migrations); err != nil {
			return fmt.Errorf("%s 迁移失败：%w", m.Name, err)
		}
		log.Info("迁移完成", "subsystem", m.Name, "database", m.Database, "scope", scopes[m.Name], "cell", spec.Cell)
	}
	return nil
}

func run(ctx context.Context, log *slog.Logger, role string) error {
	shutdown, err := tracing.Setup(ctx, "platform", role)
	if err != nil {
		return err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(sctx)
	}()
	brokers := strings.Split(os.Getenv("MIAOQIA_KAFKA_BROKERS"), ",")
	if role == "outbox-relay" {
		return runRelay(ctx, log, brokers)
	}
	spec, scopes, inventory, err := loadRole(role)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, s := range spec.Subsystems {
		want[s] = true
	}
	// 被加载子系统依赖的接口：本地绑定由提供方子系统随本进程启动时登记；配置为远程绑定的子系统视为已拆出，
	// 不在本进程加载，调用经其远程客户端
	bindings := runtime.NewBindings()
	for sub, r := range remotes {
		if os.Getenv("MIAOQIA_BINDING_"+strings.ToUpper(strings.ReplaceAll(sub, "-", "_"))) == "remote" {
			c, err := r.client(os.Getenv(envKey(sub, "URL")))
			if err != nil {
				return err
			}
			bindings.Provide(r.binding, c)
			if sub == identityapi.Name {
				resolver, ok := c.(identityapi.SessionResolver)
				if !ok {
					return fmt.Errorf("identity remote missing session resolver")
				}
				bindings.Provide(identityapi.SessionBinding, resolver)
				directory, ok := c.(identityapi.Directory)
				if !ok {
					return fmt.Errorf("identity remote missing directory")
				}
				bindings.Provide(identityapi.DirectoryBinding, directory)
				sessions, ok := c.(identityapi.ChannelSessions)
				if !ok {
					return fmt.Errorf("identity remote missing channel sessions")
				}
				bindings.Provide(identityapi.ChannelSessionsBinding, sessions)
			}
			delete(want, sub)
			log.Info("远程绑定", "subsystem", sub, "url", os.Getenv(envKey(sub, "URL")))
		} else {
			want[sub] = true
		}
	}
	for name := range want {
		if _, exists := scopes[name]; exists {
			continue
		}
		entry := inventory.Subsystems[name]
		if entry.Unit != "platform" {
			return fmt.Errorf("本地绑定 %s 不属于 platform", name)
		}
		scope, err := runtime.InstanceScope(name, nil, spec.Cell, entry.DataScopes)
		if err != nil {
			return err
		}
		scopes[name] = scope
	}
	var loaded []runtime.Module
	var envs []*runtime.Env
	for _, mk := range modules {
		m := mk()
		if !want[m.Name] {
			continue
		}
		env := &runtime.Env{DataScope: scopes[m.Name], Cell: spec.Cell, Role: role, Brokers: brokers, Log: log.With("subsystem", m.Name), Bindings: bindings,
			Config: func(k string) string { return os.Getenv(envKey(m.Name, k)) }}
		if m.Database != "" && (m.Consumers != nil || m.Routes != nil || m.Start != nil) {
			d, err := db.Open(ctx, os.Getenv(envKey(m.Name, "DB_DSN")))
			if err != nil {
				return fmt.Errorf("%s 数据库：%w", m.Name, err)
			}
			defer d.Close()
			env.DB = d
		}
		if m.Start != nil {
			if err := m.Start(ctx, env); err != nil {
				return fmt.Errorf("%s 启动失败：%w", m.Name, err)
			}
		}
		loaded, envs = append(loaded, m), append(envs, env)
	}
	names := make([]string, 0, len(loaded))
	for _, m := range loaded {
		names = append(names, m.Name)
	}
	sort.Strings(names)
	log.Info("进程角色启动", "role", role, "subsystems", names)

	switch role {
	case "api", "public-search-api":
		return serveAPI(ctx, log, loaded, envs)
	case "consumer", "public-search-consumer":
		errc := make(chan error, 16)
		n := 0
		for i, m := range loaded {
			if m.Consumers == nil {
				continue
			}
			for _, c := range m.Consumers(envs[i]) {
				n++
				go func() { errc <- c.Run(ctx) }()
			}
		}
		log.Info("事件消费者", "count", n)
		for range n {
			if err := <-errc; err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
		}
		<-ctx.Done()
		return nil
	default:
		<-ctx.Done() // scheduler、workflow-worker 的任务随所属里程碑加入
		return nil
	}
}

func serveAPI(ctx context.Context, log *slog.Logger, loaded []runtime.Module, envs []*runtime.Env) error {
	public, internal := http.NewServeMux(), http.NewServeMux()
	for i, m := range loaded {
		if m.Routes != nil {
			m.Routes(envs[i], public, internal)
		}
	}
	var resolver identityapi.SessionResolver
	for _, env := range envs {
		if value, ok := runtime.Lookup[identityapi.SessionResolver](env.Bindings, identityapi.SessionBinding); ok {
			resolver = value
			break
		}
	}
	pub, err := gateway.Public(ctx, os.Getenv("MIAOQIA_GATEWAY_ISSUER"), os.Getenv("MIAOQIA_GATEWAY_AUDIENCE"), public, resolver)
	if err != nil {
		return err
	}
	addr := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	servers := []*http.Server{
		{Addr: addr("MIAOQIA_HTTP_ADDR", ":8090"), Handler: pub, ReadHeaderTimeout: 10 * time.Second},
		{Addr: addr("MIAOQIA_INTERNAL_ADDR", ":8091"), Handler: internal, ReadHeaderTimeout: 10 * time.Second},
	}
	if !transportsecurity.Development() {
		tlsConfig, err := transportsecurity.Config("MIAOQIA_INTERNAL_TLS", true)
		if err != nil {
			return err
		}
		if os.Getenv("MIAOQIA_INTERNAL_ALLOWED_PEERS") == "" {
			return fmt.Errorf("missing internal peer allowlist")
		}
		servers[1].TLSConfig = tlsConfig
		servers[0].TLSConfig = &tls.Config{MinVersion: tls.VersionTLS13}
		for _, key := range []string{"MIAOQIA_PUBLIC_TLS_CERT", "MIAOQIA_PUBLIC_TLS_KEY"} {
			if os.Getenv(key) == "" {
				return fmt.Errorf("missing %s", key)
			}
		}
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, s := range servers {
			_ = s.Shutdown(sctx)
		}
	}()
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			if transportsecurity.Development() {
				errc <- s.ListenAndServe()
			} else if s == servers[0] {
				errc <- s.ListenAndServeTLS(os.Getenv("MIAOQIA_PUBLIC_TLS_CERT"), os.Getenv("MIAOQIA_PUBLIC_TLS_KEY"))
			} else {
				errc <- s.ListenAndServeTLS("", "")
			}
		}()
		log.Info("监听", "addr", s.Addr)
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	return nil
}

func runRelay(ctx context.Context, log *slog.Logger, brokers []string) error {
	var reg registry
	if err := readYAML(filepath.Join(deployDir(), "subsystems.yaml"), &reg); err != nil {
		return err
	}
	tmpl := os.Getenv("MIAOQIA_RELAY_DSN_TEMPLATE")
	if !strings.Contains(tmpl, "{db}") {
		return fmt.Errorf("MIAOQIA_RELAY_DSN_TEMPLATE 必须包含 {db}")
	}
	var sources []relay.Source
	for name, s := range reg.Subsystems {
		if s.Database != "" {
			sources = append(sources, relay.Source{Name: name, DSN: strings.ReplaceAll(tmpl, "{db}", s.Database)})
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Name < sources[j].Name })
	prod, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.RecordPartitioner(kgo.StickyKeyPartitioner(nil)))
	if err != nil {
		return err
	}
	defer prod.Close()
	log.Info("发件箱中继启动", "databases", len(sources))
	return (&relay.Relay{Sources: sources, Producer: prod, Log: log}).Run(ctx)
}
