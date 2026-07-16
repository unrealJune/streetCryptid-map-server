// Command streetcryptid-map-server is the single binary behind the map API,
// the tile bootstrap init container, and the tile updater sidecar.
//
// Subcommands:
//
//	serve            run the public HTTP API
//	tiles bootstrap  activate/verify a release on the tile volume (init container)
//	tiles watch      poll the signed manifest and stage updates (updater sidecar)
//	tiles verify     verify the active local release and exit
//
// The privacy boundary lives in package privacy as compile-time constants and is
// unreachable from configuration.
package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/junephilip/streetcryptid-map-server/internal/cache"
	"github.com/junephilip/streetcryptid-map-server/internal/httpapi"
	"github.com/junephilip/streetcryptid-map-server/internal/martin"
	"github.com/junephilip/streetcryptid-map-server/internal/tilesync"
)

// Injected via -ldflags at build time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: streetcryptid-map-server <serve|tiles>")
		os.Exit(2)
	}

	var err error
	switch args[0] {
	case "serve":
		err = runServe(log)
	case "tiles":
		err = runTiles(log, args[1:])
	case "version", "--version", "-v":
		fmt.Printf("streetcryptid-map-server %s (commit %s, built %s)\n", version, commit, date)
		return
	default:
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil {
		log.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}

// --- serve ---

func runServe(log *slog.Logger) error {
	martinURL := mustEnv("MARTIN_URL")
	source := lastSegment(martinURL)
	catalogURL := envOr("MARTIN_CATALOG_URL", deriveCatalogURL(martinURL))

	client := martin.New(martin.Config{
		BaseURL:      martinURL,
		Timeout:      envDuration("BUNDLE_REQUEST_TIMEOUT", 60*time.Second),
		MaxTileBytes: envInt64("BUNDLE_MAX_BYTES", 64*1024*1024),
	})

	cch, err := cache.New(os.Getenv("BUNDLE_CACHE_DIR"), envInt64("BUNDLE_CACHE_MAX_BYTES", 512*1024*1024))
	if err != nil {
		return fmt.Errorf("cache: %w", err)
	}

	datasetVersion := deriveDatasetVersion(log)

	srv := httpapi.New(httpapi.Config{
		MartinBaseURL:    martinURL,
		MartinCatalogURL: catalogURL,
		Source:           source,
		DatasetVersion:   datasetVersion,
		BundleWorkers:    int(envInt64("BUNDLE_WORKERS", 16)),
		BundleTimeout:    envDuration("BUNDLE_REQUEST_TIMEOUT", 60*time.Second),
		RatePerSec:       envFloat("RATE_LIMIT_PER_SEC", 20),
		Burst:            envFloat("RATE_LIMIT_BURST", 120),
		Logger:           log,
	}, client, cch)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv.StartSweeper(ctx)

	httpSrv := &http.Server{
		Addr:         envOr("LISTEN_ADDR", ":8080"),
		Handler:      srv.Handler(),
		ReadTimeout:  envDuration("HTTP_READ_TIMEOUT", 15*time.Second),
		WriteTimeout: envDuration("HTTP_WRITE_TIMEOUT", 90*time.Second),
		IdleTimeout:  envDuration("HTTP_IDLE_TIMEOUT", 60*time.Second),
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Info("map api listening", "addr", httpSrv.Addr, "dataset", datasetVersion, "version", version)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("shutdown complete")
	return nil
}

// deriveDatasetVersion reads the active local manifest for TILESET_VERSION. It
// is derived, never configured, so a bundle cache namespace tracks the data.
func deriveDatasetVersion(log *slog.Logger) string {
	dataDir := os.Getenv("TILE_DATA_DIR")
	if dataDir == "" {
		return "unknown"
	}
	st := tilesync.NewStore(dataDir, nil)
	ptr, err := st.ReadActivePointer()
	if err != nil {
		log.Warn("no active dataset pointer; using 'unknown'", "err", err.Error())
		return "unknown"
	}
	return ptr.Version
}

// --- tiles subcommands ---

func runTiles(log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tiles <bootstrap|watch|verify>")
	}
	cfg, err := loadTileConfig(log)
	if err != nil {
		return err
	}

	switch args[0] {
	case "bootstrap":
		s := tilesync.NewSyncer(cfg, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		return s.Bootstrap(ctx)

	case "watch":
		kube, err := tilesync.InClusterKubeClient()
		if err != nil {
			return fmt.Errorf("watch requires in-cluster access: %w", err)
		}
		if cfg.Namespace == "" {
			cfg.Namespace = kube.Namespace()
		}
		s := tilesync.NewSyncer(cfg, kube)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		log.Info("tile updater started", "interval", cfg.UpdateInterval.String(), "deployment", cfg.DeploymentName)
		if err := s.Watch(ctx); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil

	case "verify":
		s := tilesync.NewSyncer(cfg, nil)
		m, err := s.Store().ReadActive()
		if err != nil {
			return fmt.Errorf("verify: no valid active release: %w", err)
		}
		fmt.Printf("active release OK: version=%s zoom=%d-%d schema=%s\n", m.Version, m.MinZoom, m.MaxZoom, m.TileSchema)
		return nil

	case "import":
		// tiles import <pmtiles-path> [version]  — install a locally-baked file
		// as the active release (in-cluster bake mode). Optionally triggers a pod
		// recreate when POD_NAMESPACE/DEPLOYMENT_NAME are set.
		if len(args) < 2 {
			return errors.New("usage: tiles import <pmtiles-path> [version]")
		}
		path := args[1]
		importVersion := ""
		if len(args) >= 3 {
			importVersion = args[2]
		}
		var kube *tilesync.KubeClient
		if cfg.DeploymentName != "" {
			if k, err := tilesync.InClusterKubeClient(); err == nil {
				kube = k
				if cfg.Namespace == "" {
					cfg.Namespace = k.Namespace()
				}
			} else {
				log.Warn("import: no in-cluster client; will not trigger pod recreate", "err", err.Error())
			}
		}
		s := tilesync.NewSyncer(cfg, kube)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if _, err := s.ImportLocal(ctx, path, importVersion); err != nil {
			return fmt.Errorf("import: %w", err)
		}
		return nil

	default:
		return fmt.Errorf("unknown tiles subcommand %q", args[0])
	}
}

func loadTileConfig(log *slog.Logger) (tilesync.Config, error) {
	dataDir := mustEnv("TILE_DATA_DIR")
	var pub ed25519.PublicKey
	if keyFile := os.Getenv("TILE_MANIFEST_PUBLIC_KEY_FILE"); keyFile != "" {
		p, err := tilesync.LoadPublicKey(keyFile)
		if err != nil {
			return tilesync.Config{}, fmt.Errorf("public key: %w", err)
		}
		pub = p
	}
	var token string
	if tf := os.Getenv("TILE_AUTH_TOKEN_FILE"); tf != "" {
		b, err := os.ReadFile(tf)
		if err != nil {
			return tilesync.Config{}, fmt.Errorf("auth token: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	return tilesync.Config{
		DataDir:        dataDir,
		ManifestURL:    os.Getenv("TILE_MANIFEST_URL"),
		PublicKey:      pub,
		AuthToken:      token,
		UpdateInterval: envDuration("TILE_UPDATE_INTERVAL", 6*time.Hour),
		RetainReleases: int(envInt64("TILE_RETAIN_RELEASES", 2)),
		Namespace:      os.Getenv("POD_NAMESPACE"),
		DeploymentName: os.Getenv("DEPLOYMENT_NAME"),
		AllowEmpty:     os.Getenv("TILE_ALLOW_EMPTY") == "true",
		Logger:         log,
	}, nil
}

// --- env helpers ---

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "required env %s is not set\n", key)
		os.Exit(1)
	}
	return v
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func lastSegment(u string) string {
	u = strings.TrimRight(u, "/")
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[i+1:]
	}
	return u
}

// deriveCatalogURL turns http://host:3000/planet into http://host:3000/catalog.
func deriveCatalogURL(martinURL string) string {
	u := strings.TrimRight(martinURL, "/")
	if i := strings.LastIndex(u, "/"); i >= 0 {
		return u[:i] + "/catalog"
	}
	return u + "/catalog"
}
