package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/superailabs/miaoqia/services/kernel/consumer"
	"github.com/superailabs/miaoqia/services/kernel/dataread"
	"github.com/superailabs/miaoqia/services/kernel/db"
	"github.com/superailabs/miaoqia/services/kernel/reqctx"
	"github.com/superailabs/miaoqia/services/kernel/runtime"
	"github.com/superailabs/miaoqia/services/kernel/transportsecurity"
	identityapi "github.com/superailabs/miaoqia/services/subsystems/identity/api"
	identityremote "github.com/superailabs/miaoqia/services/subsystems/identity/api/remote"
	"github.com/superailabs/miaoqia/services/subsystems/search"
	searchapi "github.com/superailabs/miaoqia/services/subsystems/search/api"
	"github.com/superailabs/miaoqia/services/subsystems/search/publicmigrations"
)

const (
	publicSearchAPI      = "public-search-api"
	publicSearchConsumer = "public-search-consumer"
	publicSearchWorker   = "public-search-workflow-worker"
	publicSearchDB       = "sea_public"
	publicSearchApp      = "sea_public_app"
	publicSearchOwner    = "sea_public_owner"
	publicSearchPrefix   = "MIAOQIA_PUBLIC_SEARCH_"

	publicProjectorBinding = "search.PublicProjector"
)

var publicGeneration = regexp.MustCompile(`^[0-9a-f]{32}$`)

func isPublicSearchRole(role string) bool {
	return role == publicSearchAPI || role == publicSearchConsumer || role == publicSearchWorker
}

func publicSearchDSN(key, user string) (string, error) {
	dsn := os.Getenv(publicSearchPrefix + key)
	if dsn == "" {
		return "", fmt.Errorf("missing %s", publicSearchPrefix+key)
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil || config.Database != publicSearchDB || config.User != user ||
		config.RuntimeParams["role"] != "" || config.RuntimeParams["options"] != "" {
		return "", fmt.Errorf("%s requires database %s and role %s", publicSearchPrefix+key, publicSearchDB, user)
	}
	return dsn, nil
}

func migratePublicSearch(ctx context.Context, log *slog.Logger) error {
	dsn, err := publicSearchDSN("DB_OWNER_DSN", publicSearchOwner)
	if err != nil {
		return err
	}
	if err := db.Migrate(ctx, dsn, publicSearchApp, publicmigrations.FS); err != nil {
		return fmt.Errorf("public search migration: %w", err)
	}
	log.Info("迁移完成", "subsystem", "search", "database", publicSearchDB, "scope", "public")
	return nil
}

func assemblePublicSearch(ctx context.Context, log *slog.Logger, role string, brokers []string) ([]runtime.Module, []*runtime.Env, func(), error) {
	closeAll := func() {}
	env := &runtime.Env{
		DataScope: "public", Role: role, Brokers: brokers, Log: log.With("subsystem", "search"),
		Bindings: runtime.NewBindings(),
		Config:   func(key string) string { return os.Getenv(publicSearchPrefix + key) },
	}
	module := runtime.Module{Name: "search", Database: publicSearchDB, Migrations: publicmigrations.FS}

	if role == publicSearchAPI {
		index, err := publicSearchIndex(env.Config)
		if err != nil {
			return nil, nil, closeAll, err
		}
		if err := bindPublicSearchQuery(env, index); err != nil {
			return nil, nil, closeAll, err
		}
		module.Routes = registerPublicSearchRoutes
		return []runtime.Module{module}, []*runtime.Env{env}, closeAll, nil
	}

	dsn, err := publicSearchDSN("DB_DSN", publicSearchApp)
	if err != nil {
		return nil, nil, closeAll, err
	}
	if role == publicSearchWorker && !publicGeneration.MatchString(env.Config("GENERATION")) {
		return nil, nil, closeAll, errors.New(publicSearchPrefix + "GENERATION requires a fixed public index generation")
	}
	if role == publicSearchConsumer && (len(brokers) == 0 || strings.TrimSpace(brokers[0]) == "") {
		return nil, nil, closeAll, errors.New("public search consumer requires MIAOQIA_KAFKA_BROKERS")
	}
	database, err := openDB(ctx, dsn)
	if err != nil {
		return nil, nil, closeAll, fmt.Errorf("public search database: %w", err)
	}
	env.DB = database
	closeAll = database.Close
	if role == publicSearchConsumer {
		module.Consumers = func(env *runtime.Env) []*consumer.Consumer {
			consumers := search.PublicConsumers(database, env.Brokers)
			for _, c := range consumers {
				c.Log = env.Log
			}
			return consumers
		}
		return []runtime.Module{module}, []*runtime.Env{env}, closeAll, nil
	}
	projector, err := publicSearchProjector(database, env.Config)
	if err != nil {
		return nil, nil, closeAll, err
	}
	env.Bindings.Provide(publicProjectorBinding, projector)
	return []runtime.Module{module}, []*runtime.Env{env}, closeAll, nil
}

func publicSearchProjector(database *db.DB, config func(string) string) (*search.PublicProjector, error) {
	index, err := publicSearchIndex(config)
	if err != nil {
		return nil, err
	}
	source, err := newPublicProjectionSource(config("OWNER_EXPORT_URL"))
	if err != nil {
		return nil, err
	}
	return search.NewPublicProjector(database, index, source)
}

func bindPublicSearchQuery(env *runtime.Env, index *search.PublicOpenSearch) error {
	identityURL := env.Config("IDENTITY_URL")
	if _, err := authenticatedHTTPSURL(identityURL); err != nil {
		return errors.New(publicSearchPrefix + "IDENTITY_URL requires an authenticated HTTPS endpoint")
	}
	identity, err := identityremote.NewAuthorizer(identityURL)
	if err != nil {
		return err
	}
	resolver, ok := identity.(identityapi.SessionResolver)
	if !ok {
		return errors.New("public search requires a remote identity session resolver")
	}
	reader, err := dataread.New(env.Config("DATA_API_URL"))
	if err != nil {
		return fmt.Errorf("public search data-api binding: %w", err)
	}
	key, err := base64.StdEncoding.Strict().DecodeString(env.Config("CURSOR_KEY"))
	if err != nil || len(key) != 32 {
		return errors.New(publicSearchPrefix + "CURSOR_KEY requires 32 bytes encoded in base64")
	}
	discovery, err := publicDiscoverySources(env.Config("DISCOVERY_SOURCES"))
	if err != nil {
		return err
	}
	searcher, err := search.NewPublicSearcher(index, publicSearchAuthority{reader: reader, discovery: discovery}, key)
	if err != nil {
		return err
	}
	env.Bindings.Provide(identityapi.SessionBinding, resolver)
	env.Bindings.Provide(searchapi.PublicSearchBinding, searcher)
	return nil
}

func authenticatedHTTPSURL(endpoint string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("public search requires an authenticated HTTPS service endpoint")
	}
	return u, nil
}

func publicSearchIndex(config func(string) string) (*search.PublicOpenSearch, error) {
	endpoint := config("OPENSEARCH_URL")
	if _, err := authenticatedHTTPSURL(endpoint); err != nil {
		return nil, fmt.Errorf("%sOPENSEARCH_URL: %w", publicSearchPrefix, err)
	}
	tlsConfig, err := transportsecurity.Config(publicSearchPrefix+"OPENSEARCH_TLS", false)
	if err != nil {
		return nil, fmt.Errorf("public search OpenSearch credentials: %w", err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}, Timeout: 5 * time.Second}
	return search.NewPublicOpenSearch(endpoint, config("NAMESPACE"), client)
}

func newPublicProjectionSource(endpoint string) (*publicProjectionSource, error) {
	base, err := authenticatedHTTPSURL(endpoint)
	if err != nil {
		return nil, fmt.Errorf("%sOWNER_EXPORT_URL: %w", publicSearchPrefix, err)
	}
	if base.Path != "" && base.Path != "/" {
		return nil, errors.New(publicSearchPrefix + "OWNER_EXPORT_URL requires a service origin")
	}
	tlsConfig, err := transportsecurity.Config("MIAOQIA_INTERNAL_TLS", false)
	if err != nil {
		return nil, fmt.Errorf("public search owner export mTLS: %w", err)
	}
	tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.VerifiedChains) > 0 && len(state.PeerCertificates) > 0 {
			ids := state.PeerCertificates[0].URIs
			if len(ids) == 1 && ids[0].String() == "spiffe://miaoqia/service/public-search-export" {
				return nil
			}
		}
		return errors.New("public search owner export identity rejected")
	}
	return &publicProjectionSource{
		endpoint: base.JoinPath("/internal/v1/search-projection/mutations").String(),
		client: &http.Client{
			Transport:     &http.Transport{TLSClientConfig: tlsConfig},
			Timeout:       5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func runPublicSearchWorker(ctx context.Context, env *runtime.Env) error {
	projector, ok := runtime.Lookup[*search.PublicProjector](env.Bindings, publicProjectorBinding)
	if !ok {
		return errors.New("public search projector not bound")
	}
	generation := env.Config("GENERATION")
	ctx = reqctx.Service(ctx, publicSearchWorker)
	for {
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		advanced, err := projector.ApplyNext(attempt, generation)
		cancel()
		if err != nil {
			return fmt.Errorf("public search apply generation %s: %w", generation, err)
		}
		if advanced {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
