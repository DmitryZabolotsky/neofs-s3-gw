package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"time"

	minio "github.com/minio/minio/legacy"
	"github.com/minio/minio/legacy/config"
	"github.com/minio/minio/neofs/layer"
	"github.com/minio/minio/neofs/pool"
	"github.com/minio/minio/pkg/auth"
	"github.com/nspcc-dev/neofs-api-go/refs"
	crypto "github.com/nspcc-dev/neofs-crypto"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc/keepalive"
)

type (
	App struct {
		cli pool.Pool
		log *zap.Logger
		cfg *viper.Viper
		tls *tlsConfig
		obj minio.ObjectLayer

		conTimeout time.Duration
		reqTimeout time.Duration

		reBalance time.Duration

		webDone chan struct{}
		wrkDone chan struct{}
	}

	tlsConfig struct {
		KeyFile  string
		CertFile string
	}
)

func newApp(l *zap.Logger, v *viper.Viper) *App {
	var (
		err error
		wif string
		cli pool.Pool
		tls *tlsConfig
		uid refs.OwnerID
		obj minio.ObjectLayer

		key = fetchKey(l, v)

		reBalance = defaultRebalanceTimer

		conTimeout = defaultConnectTimeout
		reqTimeout = defaultRequestTimeout
	)

	if v.IsSet(cfgTLSKeyFile) && v.IsSet(cfgTLSCertFile) {
		tls = &tlsConfig{
			KeyFile:  v.GetString(cfgTLSKeyFile),
			CertFile: v.GetString(cfgTLSCertFile),
		}
	}

	if v := v.GetDuration(cfgConnectTimeout); v > 0 {
		conTimeout = v
	}

	if v := v.GetDuration(cfgRequestTimeout); v > 0 {
		reqTimeout = v
	}

	poolConfig := &pool.Config{
		ConnectionTTL:  v.GetDuration(cfgConnectionTTL),
		ConnectTimeout: v.GetDuration(cfgConnectTimeout),
		RequestTimeout: v.GetDuration(cfgRequestTimeout),

		Peers: fetchPeers(l, v),

		Logger:     l,
		PrivateKey: key,

		GRPCLogger:  gRPCLogger(l),
		GRPCVerbose: v.GetBool(cfgGRPCVerbose),

		ClientParameters: keepalive.ClientParameters{},
	}

	if v := v.GetDuration(cfgRebalanceTimer); v > 0 {
		reBalance = v
	}

	if cli, err = pool.New(poolConfig); err != nil {
		l.Fatal("could not prepare pool connections",
			zap.Error(err))
	}

	{ // should establish connection with NeoFS Storage Nodes
		ctx, cancel := context.WithTimeout(context.Background(), conTimeout)
		defer cancel()

		cli.ReBalance(ctx)

		if _, err = cli.GetConnection(ctx); err != nil {
			l.Fatal("could not establish connection",
				zap.Error(err))
		}
	}

	{ // should prepare object layer
		if uid, err = refs.NewOwnerID(&key.PublicKey); err != nil {
			l.Fatal("could not fetch OwnerID",
				zap.Error(err))
		}

		if wif, err = crypto.WIFEncode(key); err != nil {
			l.Fatal("could not encode key to WIF",
				zap.Error(err))
		}

		{ // Temporary solution, to resolve problems with MinIO GW access/secret keys:
			if err = os.Setenv(config.EnvAccessKey, uid.String()); err != nil {
				l.Fatal("could not set "+config.EnvAccessKey,
					zap.Error(err))
			} else if err = os.Setenv(config.EnvSecretKey, wif); err != nil {
				l.Fatal("could not set "+config.EnvSecretKey,
					zap.Error(err))
			}

			l.Info("used credentials",
				zap.String("AccessKey", uid.String()),
				zap.String("SecretKey", wif))
		}

		if obj, err = layer.NewLayer(cli, l, auth.Credentials{AccessKey: uid.String(), SecretKey: wif}); err != nil {
			l.Fatal("could not prepare ObjectLayer",
				zap.Error(err))
		}
	}

	return &App{
		cli: cli,
		log: l,
		cfg: v,
		obj: obj,
		tls: tls,

		webDone: make(chan struct{}, 1),
		wrkDone: make(chan struct{}, 1),

		reBalance: reBalance,

		conTimeout: conTimeout,
		reqTimeout: reqTimeout,
	}
}

func (a *App) Wait() {
	a.log.Info("application started")

	select {
	case <-a.wrkDone: // wait for worker is stopped
		<-a.webDone
	case <-a.webDone: // wait for web-server is stopped
		<-a.wrkDone
	}

	a.log.Info("application finished")
}

func (a *App) Server(ctx context.Context) {
	var (
		err  error
		lis  net.Listener
		lic  net.ListenConfig
		srv  = new(http.Server)
		addr = a.cfg.GetString(cfgListenAddress)
	)

	if lis, err = lic.Listen(ctx, "tcp", addr); err != nil {
		a.log.Fatal("could not prepare listener",
			zap.Error(err))
	}

	router := newS3Router()

	// Attach app-specific routes:
	attachHealthy(router, a.cli)
	attachMetrics(router, a.cfg, a.log)
	attachProfiler(router, a.cfg, a.log)

	// Attach S3 API:
	minio.AttachS3API(router, a.obj, a.log)

	// Use mux.Router as http.Handler
	srv.Handler = router

	go func() {
		a.log.Info("starting server",
			zap.String("bind", addr))

		switch a.tls {
		case nil:
			if err = srv.Serve(lis); err != nil && err != http.ErrServerClosed {
				a.log.Fatal("listen and serve",
					zap.Error(err))
			}
		default:
			a.log.Info("using certificate",
				zap.String("key", a.tls.KeyFile),
				zap.String("cert", a.tls.CertFile))

			if err = srv.ServeTLS(lis, a.tls.CertFile, a.tls.KeyFile); err != nil && err != http.ErrServerClosed {
				a.log.Fatal("listen and serve",
					zap.Error(err))
			}
		}
	}()

	<-ctx.Done()

	ctx, cancel := context.WithTimeout(context.Background(), defaultShutdownTimeout)
	defer cancel()

	a.log.Info("stopping server",
		zap.Error(srv.Shutdown(ctx)))

	close(a.webDone)
}

func (a *App) Worker(ctx context.Context) {
	tick := time.NewTimer(a.reBalance)

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-tick.C:
			ctx, cancel := context.WithTimeout(ctx, a.conTimeout)
			a.cli.ReBalance(ctx)
			cancel()

			tick.Reset(a.reBalance)
		}
	}

	tick.Stop()
	a.cli.Close()
	a.log.Info("stopping worker")
	close(a.wrkDone)
}