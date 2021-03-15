package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/handlers"
	"github.com/patrickmn/go-cache"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/superfly/flyctl/api"
)

type ctxKey string

const (
	appNameKey     = ctxKey("app-name")
	accessTokenKey = ctxKey("access-token")
)

var (
	orgSlug         = os.Getenv("ALLOW_ORG_SLUG")
	log             = logrus.New()
	maxIdleDuration = 10 * time.Minute
	jobDeadline     = time.NewTimer(maxIdleDuration)
	buildsWg        sync.WaitGroup
	authCache       = cache.New(5*time.Minute, 10*time.Minute)

	// dev and testing
	noDockerd = os.Getenv("NO_DOCKERD") == "1"
	noAuth    = os.Getenv("NO_AUTH") == "1"

	// build variables
	gitSha    string
	buildTime string
)

func main() {
	lvl, err := logrus.ParseLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		lvl = logrus.InfoLevel
	}
	log.SetLevel(lvl)
	log.SetFormatter(&logrus.TextFormatter{
		TimestampFormat: "2006-01-02T15:04:05.000000000Z07:00",
		FullTimestamp:   true,
	})

	log.Infof("Build SHA:%s Time:%s", gitSha, buildTime)

	api.SetBaseURL("https://api.fly.io")

	ctx, cancel := context.WithCancel(context.Background())

	stopDockerdFn, err := runDockerd()
	if err != nil {
		log.Fatalln(err)
	}

	httpServer := &http.Server{
		Addr:    ":8080",
		Handler: handlers.LoggingHandler(log.Writer(), authRequest(resetDeadline(proxy()))),

		// reuse the context we've created
		BaseContext: func(_ net.Listener) context.Context { return ctx },
	}

	// Run server
	go func() {
		log.Infof("Listening on %s", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			// it is fine to use Fatal here because it is not main gorutine
			log.Fatalf("HTTP server ListenAndServe: %v", err)
		}
	}()

	killSig := make(chan os.Signal, 1)

	signal.Notify(
		killSig,
		syscall.SIGINT,
	)

	var killSignaled bool

	keepAliveSig := make(chan os.Signal, 1)
	signal.Notify(
		keepAliveSig,
		syscall.SIGUSR1,
	)

ALIVE:
	for {
		select {
		case <-keepAliveSig:
			log.Info("received SIGUSR1, resetting job deadline")
			jobDeadline.Reset(maxIdleDuration)
		case <-jobDeadline.C:
			log.Info("Deadline reached without docker build - shutting down...")
			break ALIVE
		case <-killSig:
			killSignaled = true
			log.Info("os.Interrupt - gracefully shutting down...")
			go func() {
				<-killSig
				log.Fatal("os.Kill - abruptly terminating...")
			}()
			break ALIVE
		}
	}

	log.Info("shutting down")

	gracefullCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()

	if err := httpServer.Shutdown(gracefullCtx); err != nil {
		log.Warnf("shutdown error: %v\n", err)
		defer os.Exit(1)
		return
	} else {
		log.Infof("gracefully stopped\n")
	}

	if killSignaled {
		log.Info("Waiting for builds to finish (reason: killSignaled)")
		buildsWg.Wait()
	}

	stopDockerdFn()

	// manually cancel context if not using httpServer.RegisterOnShutdown(cancel)
	cancel()

	defer os.Exit(0)
}

func runDockerd() (func(), error) {
	// noop
	if noDockerd {
		return func() {}, nil
	}

	// Launch `dockerd`
	dockerd := exec.Command("dockerd", "-p", "/var/run/docker.pid")
	dockerd.Stdout = os.Stderr
	dockerd.Stderr = os.Stderr

	if err := dockerd.Start(); err != nil {
		return nil, errors.Wrap(err, "could not start dockerd")
	}

	cmd := exec.Command("docker", "buildx", "inspect", "--bootstrap")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Warnln("Error bootstrapping buildx builder:", err)
	}

	// This horrible hack attempts to fix timeouts when clients make buildkit requests before the
	// default builder is fully started. eg "FIXME: Got an API for which error does not match any expected type!!!: context canceled"
	// delaying by a few seconds seems to help
	time.Sleep(2 * time.Second)

	dockerDone := make(chan struct{})

	go func() {
		if err := dockerd.Wait(); err != nil {
			log.Errorf("error waiting on docker: %v", err)
		}
		close(dockerDone)
		log.Info("dockerd has exited")
	}()

	stopFn := func() {
		dockerProc := dockerd.Process
		if dockerProc != nil {
			if err := dockerProc.Signal(os.Interrupt); err != nil {
				log.Errorf("error signaling dockerd to interrupt: %v", err)
			} else {
				log.Info("Waiting for dockerd to exit")
				<-dockerDone
			}
		}
	}

	return stopFn, nil
}

// proxy to docker sock, by hijacking the connection
func proxy() http.Handler {
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = "http"
			r.URL.Host = "localhost"
			fmt.Println(r.URL)
		},
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				fmt.Println("dial", network, addr)
				return net.Dial("unix", "/var/run/docker.sock")
			},
		},
	}

	return proxy
}

func authRequest(next http.Handler) http.Handler {
	if noAuth {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appName, authToken, ok := r.BasicAuth()

		if !ok || !authorizeRequestWithCache(appName, authToken) {
			if err := writeDockerDaemonResponse2(w, http.StatusUnauthorized, "You are not authorized to use this builder"); err != nil {
				log.Warnln("error writing response", err)
			}
			return
		}

		next.ServeHTTP(w, r)
	})
}

func resetDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buildsWg.Add(1)
		defer buildsWg.Done()

		defer func() {
			log.Debug("resetting deadline")
			jobDeadline.Reset(maxIdleDuration)
		}()

		next.ServeHTTP(w, r)
	})
}

func writeDockerDaemonResponse2(w http.ResponseWriter, status int, message string) error {
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(map[string]string{"message": message})
}

func authorizeRequestWithCache(appName, authToken string) bool {
	if noAuth {
		return true
	}

	if appName == "" || authToken == "" {
		return false
	}

	cacheKey := appName + ":" + authToken
	if val, ok := authCache.Get(cacheKey); ok {
		if authorized, ok := val.(bool); ok {
			log.Debugln("authorized from cache")
			return authorized
		}
	}

	authorized := authorizeRequest(appName, authToken)
	authCache.Set(cacheKey, authorized, 0)
	log.Debugln("authorized from api")
	return authorized
}

func authorizeRequest(appName, authToken string) bool {
	fly := api.NewClient(authToken, "0.0.0.0.0.0.1")
	app, err := fly.GetApp(appName)
	if app == nil || err != nil {
		log.Warnf("Error fetching app %s:", appName, err)
		return false
	}

	org, err := fly.FindOrganizationBySlug(orgSlug)
	if org == nil || err != nil {
		log.Warnf("Error fetching org %s:", orgSlug, err)
		return false
	}

	if app.Organization.ID != org.ID {
		log.Warnf("App %s does not belong to org %s", app.Name, org.Slug)
		return false
	}

	return true
}
