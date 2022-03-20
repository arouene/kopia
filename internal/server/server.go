// Package server implements Kopia API server handlers.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"golang.org/x/sync/semaphore"

	"github.com/kopia/kopia/internal/auth"
	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/internal/ctxutil"
	"github.com/kopia/kopia/internal/mount"
	"github.com/kopia/kopia/internal/passwordpersist"
	"github.com/kopia/kopia/internal/serverapi"
	"github.com/kopia/kopia/internal/uitask"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/logging"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/snapshotmaintenance"
)

var log = logging.Module("kopia/server")

const (
	// maximum time between attempts to run maintenance.
	maxMaintenanceAttemptFrequency = 4 * time.Hour
	sleepOnMaintenanceError        = 30 * time.Minute

	// retry initialization of repository starting at 1s doubling delay each time up to max 5 minutes
	// (1s, 2s, 4s, 8s, 16s, 32s, 64s, 128s, 256s, 300s, 300s, 300s, ...)
	retryInitRepositorySleepOnError    = 1 * time.Second
	maxRetryInitRepositorySleepOnError = 5 * time.Minute

	kopiaAuthCookie         = "Kopia-Auth"
	kopiaAuthCookieTTL      = 1 * time.Minute
	kopiaAuthCookieAudience = "kopia"
	kopiaAuthCookieIssuer   = "kopia-server"
)

type csrfTokenOption int

const (
	csrfTokenRequired csrfTokenOption = 1 + iota
	csrfTokenNotRequired
)

type apiRequestFunc func(ctx context.Context, rc requestContext) (interface{}, *apiError)

// Server exposes simple HTTP API for programmatically accessing Kopia features.
type Server struct {
	OnShutdown      func(ctx context.Context) error
	options         Options
	authenticator   auth.Authenticator
	authorizer      auth.Authorizer
	uploadSemaphore *semaphore.Weighted

	initTaskMutex sync.Mutex
	// +checklocks:initTaskMutex
	initRepositoryTaskID string // non-empty - repository is currently being opened.

	serverMutex sync.RWMutex
	// +checklocks:serverMutex
	rep repo.Repository
	// +checklocks:serverMutex
	cancelRep context.CancelFunc // call to cancel repository
	// +checklocks:serverMutex
	sourceManagers map[snapshot.SourceInfo]*sourceManager
	// +checklocks:serverMutex
	mounts map[object.ID]mount.Controller

	taskmgr              *uitask.Manager
	authCookieSigningKey []byte

	grpcServerState
}

// SetupHTMLUIAPIHandlers registers API requests required by the HTMLUI.
func (s *Server) SetupHTMLUIAPIHandlers(m *mux.Router) {
	// sources
	m.HandleFunc("/api/v1/sources", s.handleUI(handleSourcesList)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/sources", s.handleUI(handleSourcesCreate)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/sources/upload", s.handleUI(handleUpload)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/sources/cancel", s.handleUI(handleCancel)).Methods(http.MethodPost)

	// snapshots
	m.HandleFunc("/api/v1/snapshots", s.handleUI(handleListSnapshots)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/snapshots/delete", s.handleUI(handleDeleteSnapshots)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/snapshots/edit", s.handleUI(handleEditSnapshots)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/policy", s.handleUI(handlePolicyGet)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/policy", s.handleUI(handlePolicyPut)).Methods(http.MethodPut)
	m.HandleFunc("/api/v1/policy", s.handleUI(handlePolicyDelete)).Methods(http.MethodDelete)
	m.HandleFunc("/api/v1/policy/resolve", s.handleUI(handlePolicyResolve)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/policies", s.handleUI(handlePolicyList)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/refresh", s.handleUI(handleRefresh)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/objects/{objectID}", s.requireAuth(csrfTokenNotRequired, handleObjectGet)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/restore", s.handleUI(handleRestore)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/estimate", s.handleUI(handleEstimate)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/paths/resolve", s.handleUI(handlePathResolve)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/cli", s.handleUI(handleCLIInfo)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/repo/status", s.handleUIPossiblyNotConnected(handleRepoStatus)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/repo/sync", s.handleUI(handleRepoSync)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/repo/connect", s.handleUIPossiblyNotConnected(handleRepoConnect)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/repo/exists", s.handleUIPossiblyNotConnected(handleRepoExists)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/repo/create", s.handleUIPossiblyNotConnected(handleRepoCreate)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/repo/description", s.handleUI(handleRepoSetDescription)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/repo/disconnect", s.handleUI(handleRepoDisconnect)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/repo/algorithms", s.handleUIPossiblyNotConnected(handleRepoSupportedAlgorithms)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/repo/throttle", s.handleUI(handleRepoGetThrottle)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/repo/throttle", s.handleUI(handleRepoSetThrottle)).Methods(http.MethodPut)

	m.HandleFunc("/api/v1/mounts", s.handleUI(handleMountCreate)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/mounts/{rootObjectID}", s.handleUI(handleMountDelete)).Methods(http.MethodDelete)
	m.HandleFunc("/api/v1/mounts/{rootObjectID}", s.handleUI(handleMountGet)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/mounts", s.handleUI(handleMountList)).Methods(http.MethodGet)

	m.HandleFunc("/api/v1/current-user", s.handleUIPossiblyNotConnected(handleCurrentUser)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/ui-preferences", s.handleUIPossiblyNotConnected(handleGetUIPreferences)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/ui-preferences", s.handleUIPossiblyNotConnected(handleSetUIPreferences)).Methods(http.MethodPut)

	m.HandleFunc("/api/v1/tasks-summary", s.handleUIPossiblyNotConnected(handleTaskSummary)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/tasks", s.handleUIPossiblyNotConnected(handleTaskList)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/tasks/{taskID}", s.handleUIPossiblyNotConnected(handleTaskInfo)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/tasks/{taskID}/logs", s.handleUIPossiblyNotConnected(handleTaskLogs)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/tasks/{taskID}/cancel", s.handleUIPossiblyNotConnected(handleTaskCancel)).Methods(http.MethodPost)
}

// SetupRepositoryAPIHandlers registers HTTP repository API handlers.
func (s *Server) SetupRepositoryAPIHandlers(m *mux.Router) {
	m.HandleFunc("/api/v1/flush", s.handleRepositoryAPI(anyAuthenticatedUser, handleFlush)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/repo/parameters", s.handleRepositoryAPI(anyAuthenticatedUser, handleRepoParameters)).Methods(http.MethodGet)

	m.HandleFunc("/api/v1/contents/{contentID}", s.handleRepositoryAPI(requireContentAccess(auth.AccessLevelRead), handleContentInfo)).Methods(http.MethodGet).Queries("info", "1")
	m.HandleFunc("/api/v1/contents/{contentID}", s.handleRepositoryAPI(requireContentAccess(auth.AccessLevelRead), handleContentGet)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/contents/{contentID}", s.handleRepositoryAPI(requireContentAccess(auth.AccessLevelAppend), handleContentPut)).Methods(http.MethodPut)
	m.HandleFunc("/api/v1/contents/prefetch", s.handleRepositoryAPI(requireContentAccess(auth.AccessLevelRead), handleContentPrefetch)).Methods(http.MethodPost)

	m.HandleFunc("/api/v1/manifests/{manifestID}", s.handleRepositoryAPI(handlerWillCheckAuthorization, handleManifestGet)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/manifests/{manifestID}", s.handleRepositoryAPI(handlerWillCheckAuthorization, handleManifestDelete)).Methods(http.MethodDelete)
	m.HandleFunc("/api/v1/manifests", s.handleRepositoryAPI(handlerWillCheckAuthorization, handleManifestCreate)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/manifests", s.handleRepositoryAPI(handlerWillCheckAuthorization, handleManifestList)).Methods(http.MethodGet)
}

// SetupControlAPIHandlers registers control API handlers.
func (s *Server) SetupControlAPIHandlers(m *mux.Router) {
	// server control API, requires authentication as `server-control` and no CSRF token.
	m.HandleFunc("/api/v1/control/sources", s.handleServerControlAPI(handleSourcesList)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/control/status", s.handleServerControlAPIPossiblyNotConnected(handleRepoStatus)).Methods(http.MethodGet)
	m.HandleFunc("/api/v1/control/flush", s.handleServerControlAPI(handleFlush)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/control/refresh", s.handleServerControlAPI(handleRefresh)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/control/shutdown", s.handleServerControlAPIPossiblyNotConnected(handleShutdown)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/control/trigger-snapshot", s.handleServerControlAPI(handleUpload)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/control/cancel-snapshot", s.handleServerControlAPI(handleCancel)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/control/pause-source", s.handleServerControlAPI(handlePause)).Methods(http.MethodPost)
	m.HandleFunc("/api/v1/control/resume-source", s.handleServerControlAPI(handleResume)).Methods(http.MethodPost)
}

func isAuthenticated(rc requestContext) bool {
	authn := rc.srv.getAuthenticator()
	if authn == nil {
		return true
	}

	username, password, ok := rc.req.BasicAuth()
	if !ok {
		rc.w.Header().Set("WWW-Authenticate", `Basic realm="Kopia"`)
		http.Error(rc.w, "Missing credentials.\n", http.StatusUnauthorized)

		return false
	}

	if c, err := rc.req.Cookie(kopiaAuthCookie); err == nil && c != nil {
		if rc.srv.isAuthCookieValid(username, c.Value) {
			// found a short-term JWT cookie that matches given username, trust it.
			// this avoids potentially expensive password hashing inside the authenticator.
			return true
		}
	}

	if !authn.IsValid(rc.req.Context(), rc.rep, username, password) {
		rc.w.Header().Set("WWW-Authenticate", `Basic realm="Kopia"`)
		http.Error(rc.w, "Access denied.\n", http.StatusUnauthorized)

		return false
	}

	now := clock.Now()

	ac, err := rc.srv.generateShortTermAuthCookie(username, now)
	if err != nil {
		log(rc.req.Context()).Errorf("unable to generate short-term auth cookie: %v", err)
	} else {
		http.SetCookie(rc.w, &http.Cookie{
			Name:    kopiaAuthCookie,
			Value:   ac,
			Expires: now.Add(kopiaAuthCookieTTL),
			Path:    "/",
		})
	}

	return true
}

func (s *Server) isAuthCookieValid(username, cookieValue string) bool {
	tok, err := jwt.ParseWithClaims(cookieValue, &jwt.RegisteredClaims{}, func(t *jwt.Token) (interface{}, error) {
		return s.authCookieSigningKey, nil
	})
	if err != nil {
		return false
	}

	sc, ok := tok.Claims.(*jwt.RegisteredClaims)
	if !ok {
		return false
	}

	return sc.Subject == username
}

func (s *Server) generateShortTermAuthCookie(username string, now time.Time) (string, error) {
	// nolint:wrapcheck
	return jwt.NewWithClaims(jwt.SigningMethodHS256, &jwt.RegisteredClaims{
		Subject:   username,
		NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(kopiaAuthCookieTTL)),
		IssuedAt:  jwt.NewNumericDate(now),
		Audience:  jwt.ClaimStrings{kopiaAuthCookieAudience},
		ID:        uuid.New().String(),
		Issuer:    kopiaAuthCookieIssuer,
	}).SignedString(s.authCookieSigningKey)
}

func (s *Server) captureRequestContext(w http.ResponseWriter, r *http.Request) requestContext {
	s.serverMutex.Lock()
	defer s.serverMutex.Unlock()

	return requestContext{
		w:   w,
		req: r,
		rep: s.rep,
		srv: s,
	}
}

func (s *Server) getAuthenticator() auth.Authenticator {
	return s.authenticator
}

func (s *Server) getAuthorizer() auth.Authorizer {
	return s.authorizer
}

func (s *Server) getOptions() *Options {
	return &s.options
}

func (s *Server) taskManager() *uitask.Manager {
	return s.taskmgr
}

func (s *Server) requireAuth(checkCSRFToken csrfTokenOption, f func(ctx context.Context, rc requestContext)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rc := s.captureRequestContext(w, r)

		if !isAuthenticated(rc) {
			return
		}

		if checkCSRFToken == csrfTokenRequired {
			if !s.validateCSRFToken(r) {
				http.Error(w, "Invalid or missing CSRF token.\n", http.StatusUnauthorized)
				return
			}
		}

		f(r.Context(), rc)
	}
}

func httpAuthorizationInfo(ctx context.Context, rc requestContext) auth.AuthorizationInfo {
	// authentication already done
	userAtHost, _, _ := rc.req.BasicAuth()

	authz := rc.srv.getAuthorizer().Authorize(ctx, rc.rep, userAtHost)
	if authz == nil {
		authz = auth.NoAccess()
	}

	return authz
}

type isAuthorizedFunc func(ctx context.Context, rc requestContext) bool

func (s *Server) handleServerControlAPI(f apiRequestFunc) http.HandlerFunc {
	return s.handleServerControlAPIPossiblyNotConnected(func(ctx context.Context, rc requestContext) (interface{}, *apiError) {
		if rc.rep == nil {
			return nil, requestError(serverapi.ErrorNotConnected, "not connected")
		}

		return f(ctx, rc)
	})
}

func (s *Server) handleServerControlAPIPossiblyNotConnected(f apiRequestFunc) http.HandlerFunc {
	return s.handleRequestPossiblyNotConnected(requireServerControlUser, csrfTokenNotRequired, func(ctx context.Context, rc requestContext) (interface{}, *apiError) {
		return f(ctx, rc)
	})
}

func (s *Server) handleRepositoryAPI(isAuthorized isAuthorizedFunc, f apiRequestFunc) http.HandlerFunc {
	return s.handleRequestPossiblyNotConnected(isAuthorized, csrfTokenNotRequired, func(ctx context.Context, rc requestContext) (interface{}, *apiError) {
		if rc.rep == nil {
			return nil, requestError(serverapi.ErrorNotConnected, "not connected")
		}

		return f(ctx, rc)
	})
}

func (s *Server) handleUI(f apiRequestFunc) http.HandlerFunc {
	return s.handleRequestPossiblyNotConnected(requireUIUser, csrfTokenRequired, func(ctx context.Context, rc requestContext) (interface{}, *apiError) {
		if rc.rep == nil {
			return nil, requestError(serverapi.ErrorNotConnected, "not connected")
		}

		return f(ctx, rc)
	})
}

func (s *Server) handleUIPossiblyNotConnected(f apiRequestFunc) http.HandlerFunc {
	return s.handleRequestPossiblyNotConnected(requireUIUser, csrfTokenRequired, f)
}

func (s *Server) handleRequestPossiblyNotConnected(isAuthorized isAuthorizedFunc, checkCSRFToken csrfTokenOption, f apiRequestFunc) http.HandlerFunc {
	return s.requireAuth(checkCSRFToken, func(ctx context.Context, rc requestContext) {
		// we must pre-read request body before acquiring the lock as it sometimes leads to deadlock
		// in HTTP/2 server.
		// See https://github.com/golang/go/issues/40816
		body, berr := io.ReadAll(rc.req.Body)
		if berr != nil {
			http.Error(rc.w, "error reading request body", http.StatusInternalServerError)
			return
		}
		rc.body = body

		if s.options.LogRequests {
			log(ctx).Debugf("request %v (%v bytes)", rc.req.URL, len(body))
		}

		rc.w.Header().Set("Content-Type", "application/json")

		e := json.NewEncoder(rc.w)
		e.SetIndent("", "  ")

		var v interface{}
		var err *apiError

		// process the request while ignoring the cancelation signal
		// to ensure all goroutines started by it won't be canceled
		// when the request finishes.
		ctx = ctxutil.Detach(ctx)

		if isAuthorized(ctx, rc) {
			v, err = f(ctx, rc)
		} else {
			err = accessDeniedError()
		}

		if err == nil {
			if b, ok := v.([]byte); ok {
				if _, err := rc.w.Write(b); err != nil {
					log(ctx).Errorf("error writing response: %v", err)
				}
			} else if err := e.Encode(v); err != nil {
				log(ctx).Errorf("error encoding response: %v", err)
			}

			return
		}

		rc.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		rc.w.Header().Set("X-Content-Type-Options", "nosniff")
		rc.w.WriteHeader(err.httpErrorCode)

		if s.options.LogRequests && err.apiErrorCode == serverapi.ErrorNotConnected {
			log(ctx).Debugf("%v: error code %v message %v", rc.req.URL, err.apiErrorCode, err.message)
		}

		_ = e.Encode(&serverapi.ErrorResponse{
			Code:  err.apiErrorCode,
			Error: err.message,
		})
	})
}

// Refresh refreshes the state of the server in response to external signal (e.g. SIGHUP).
func (s *Server) Refresh(ctx context.Context) error {
	s.serverMutex.Lock()
	defer s.serverMutex.Unlock()

	if s.rep == nil {
		return nil
	}

	if err := s.rep.Refresh(ctx); err != nil {
		return errors.Wrap(err, "unable to refresh repository")
	}

	if s.authenticator != nil {
		if err := s.authenticator.Refresh(ctx); err != nil {
			log(ctx).Errorf("unable to refresh authenticator: %v", err)
		}
	}

	if s.authorizer != nil {
		if err := s.authorizer.Refresh(ctx); err != nil {
			log(ctx).Errorf("unable to refresh authorizer: %v", err)
		}
	}

	if err := s.syncSourcesLocked(ctx); err != nil {
		return errors.Wrap(err, "unable to sync sources")
	}

	return nil
}

func handleRefresh(ctx context.Context, rc requestContext) (interface{}, *apiError) {
	// refresh is an alias for /repo/sync
	return handleRepoSync(ctx, rc)
}

func handleFlush(ctx context.Context, rc requestContext) (interface{}, *apiError) {
	rw, ok := rc.rep.(repo.RepositoryWriter)
	if !ok {
		return nil, repositoryNotWritableError()
	}

	if err := rw.Flush(ctx); err != nil {
		return nil, internalServerError(err)
	}

	return &serverapi.Empty{}, nil
}

func handleShutdown(ctx context.Context, rc requestContext) (interface{}, *apiError) {
	log(ctx).Infof("shutting down due to API request")

	rc.srv.requestShutdown(ctx)

	return &serverapi.Empty{}, nil
}

func (s *Server) requestShutdown(ctx context.Context) {
	if f := s.OnShutdown; f != nil {
		go func() {
			if err := f(ctx); err != nil {
				log(ctx).Errorf("shutdown failed: %v", err)
			}
		}()
	}
}

func (s *Server) beginUpload(ctx context.Context, src snapshot.SourceInfo) bool {
	log(ctx).Debugf("waiting on semaphore to upload %v", src)

	if err := s.uploadSemaphore.Acquire(ctx, 1); err != nil {
		log(ctx).Debugf("error acquiring semaphore to upload %v: %v", src, err)

		return false
	}

	log(ctx).Debugf("entered semaphore to upload %v", src)

	return true
}

func (s *Server) endUpload(ctx context.Context, src snapshot.SourceInfo) {
	log(ctx).Debugf("finished uploading %v", src)
	s.uploadSemaphore.Release(1)
}

func (s *Server) triggerRefreshSource(sourceInfo snapshot.SourceInfo) {
	s.serverMutex.RLock()
	defer s.serverMutex.RUnlock()

	sm := s.sourceManagers[sourceInfo]
	if sm == nil {
		return
	}

	select {
	case sm.refreshRequested <- struct{}{}:
	default:
	}
}

// SetRepository sets the repository (nil is allowed and indicates server that is not
// connected to the repository).
func (s *Server) SetRepository(ctx context.Context, rep repo.Repository) error {
	s.serverMutex.Lock()
	defer s.serverMutex.Unlock()

	if s.rep == rep {
		// nothing to do
		return nil
	}

	if s.rep != nil {
		s.unmountAllLocked(ctx)

		// close previous source managers
		log(ctx).Infof("stopping all source managers")
		s.stopAllSourceManagersLocked(ctx)
		log(ctx).Infof("stopped all source managers")

		if err := s.rep.Close(ctx); err != nil {
			return errors.Wrap(err, "unable to close previous repository")
		}

		cr := s.cancelRep
		s.cancelRep = nil

		if cr != nil {
			cr()
		}
	}

	s.rep = rep
	if s.rep == nil {
		return nil
	}

	if err := s.syncSourcesLocked(ctx); err != nil {
		s.stopAllSourceManagersLocked(ctx)
		s.rep = nil

		return err
	}

	ctx, s.cancelRep = context.WithCancel(ctx)
	go s.refreshPeriodically(ctx)

	if dr, ok := rep.(repo.DirectRepository); ok {
		go s.periodicMaintenance(ctx, dr)
	}

	return nil
}

func (s *Server) refreshPeriodically(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return

		case <-time.After(s.options.RefreshInterval):
			if err := s.Refresh(ctx); err != nil {
				log(ctx).Errorf("error refreshing repository: %v", err)
			}
		}
	}
}

func (s *Server) periodicMaintenance(ctx context.Context, rep repo.DirectRepository) {
	for {
		now := clock.Now()

		// this will return time == now or in the past if the maintenance is currently runnable
		// by the current user.
		nextMaintenanceTime, err := maintenance.TimeToAttemptNextMaintenance(ctx, rep, now.Add(maxMaintenanceAttemptFrequency))
		if err != nil {
			log(ctx).Debugw("unable to determine time till next maintenance", "error", err)

			if !clock.SleepInterruptibly(ctx, sleepOnMaintenanceError) {
				return
			}

			continue
		}

		// maintenance is not due yet, sleep interruptibly
		if nextMaintenanceTime.After(now) {
			log(ctx).Debugw("sleeping until next maintenance attempt", "time", nextMaintenanceTime)

			if !clock.SleepInterruptibly(ctx, nextMaintenanceTime.Sub(now)) {
				return
			}

			// we woke up after sleeping, do not run maintenance immediately, but re-check first,
			// we may have lost ownership or parameters may have changed.
			continue
		}

		if err := s.taskmgr.Run(ctx, "Maintenance", "Periodic maintenance", func(ctx context.Context, _ uitask.Controller) error {
			return periodicMaintenanceOnce(ctx, rep)
		}); err != nil {
			log(ctx).Errorf("unable to run maintenance: %v", err)

			if !clock.SleepInterruptibly(ctx, sleepOnMaintenanceError) {
				return
			}
		}
	}
}

func periodicMaintenanceOnce(ctx context.Context, rep repo.Repository) error {
	dr, ok := rep.(repo.DirectRepository)
	if !ok {
		return errors.Errorf("not a direct repository")
	}

	// nolint:wrapcheck
	return repo.DirectWriteSession(ctx, dr, repo.WriteSessionOptions{
		Purpose: "periodicMaintenanceOnce",
	}, func(ctx context.Context, w repo.DirectRepositoryWriter) error {
		// nolint:wrapcheck
		return snapshotmaintenance.Run(ctx, w, maintenance.ModeAuto, false, maintenance.SafetyFull)
	})
}

// +checklocks:s.serverMutex
func (s *Server) stopAllSourceManagersLocked(ctx context.Context) {
	for _, sm := range s.sourceManagers {
		sm.stop(ctx)
	}

	for _, sm := range s.sourceManagers {
		sm.waitUntilStopped(ctx)
	}

	s.sourceManagers = map[snapshot.SourceInfo]*sourceManager{}
}

// +checklocks:s.serverMutex
func (s *Server) syncSourcesLocked(ctx context.Context) error {
	sources := map[snapshot.SourceInfo]bool{}

	if s.rep != nil {
		snapshotSources, err := snapshot.ListSources(ctx, s.rep)
		if err != nil {
			return errors.Wrap(err, "unable to list sources")
		}

		policies, err := policy.ListPolicies(ctx, s.rep)
		if err != nil {
			return errors.Wrap(err, "unable to list sources")
		}

		for _, ss := range snapshotSources {
			sources[ss] = true
		}

		for _, pol := range policies {
			if pol.Target().Path != "" && pol.Target().Host != "" && pol.Target().UserName != "" {
				sources[pol.Target()] = true
			}
		}
	}

	// copy existing sources to a map, from which we will remove sources that are found
	// in the repository
	oldSourceManagers := map[snapshot.SourceInfo]*sourceManager{}
	for k, v := range s.sourceManagers {
		oldSourceManagers[k] = v
	}

	for src := range sources {
		if sm, ok := oldSourceManagers[src]; ok {
			// pre-existing source, already has a manager
			delete(oldSourceManagers, src)
			sm.refreshStatus(ctx)
		} else {
			sm := newSourceManager(src, s, s.rep)
			s.sourceManagers[src] = sm

			sm.start(ctx, s.rep)
		}
	}

	// whatever is left in oldSourceManagers are managers for sources that don't exist anymore.
	// stop source manager for sources no longer in the repo.
	for _, sm := range oldSourceManagers {
		sm.stop(ctx)
	}

	for src, sm := range oldSourceManagers {
		sm.waitUntilStopped(ctx)
		delete(s.sourceManagers, src)
	}

	return nil
}

func (s *Server) isKnownUIRoute(path string) bool {
	return strings.HasPrefix(path, "/snapshots") ||
		strings.HasPrefix(path, "/policies") ||
		strings.HasPrefix(path, "/tasks") ||
		strings.HasPrefix(path, "/repo")
}

func (s *Server) patchIndexBytes(sessionID string, b []byte) []byte {
	if s.options.UITitlePrefix != "" {
		b = bytes.ReplaceAll(b, []byte("<title>"), []byte("<title>"+html.EscapeString(s.options.UITitlePrefix)))
	}

	if v := repo.BuildVersion; v != "" {
		b = bytes.ReplaceAll(b, []byte(`</title>`), []byte(" v"+html.EscapeString(repo.BuildVersion)+"</title>"))
		b = bytes.ReplaceAll(b, []byte(`<p class="version-info">Version `), []byte(`<p class="version-info">Version v`+html.EscapeString(repo.BuildVersion+" "+repo.BuildInfo+" ")))
	}

	csrfToken := s.generateCSRFToken(sessionID)

	// insert <meta name="kopia-csrf-token" content="..." /> just before closing head tag.
	b = bytes.ReplaceAll(b,
		[]byte(`</head>`),
		[]byte(`<meta name="kopia-csrf-token" content="`+csrfToken+`" /></head>`))

	return b
}

func maybeReadIndexBytes(fs http.FileSystem) []byte {
	rootFile, err := fs.Open("index.html")
	if err != nil {
		return nil
	}

	defer rootFile.Close() //nolint:errcheck

	rd, err := io.ReadAll(rootFile)
	if err != nil {
		return nil
	}

	return rd
}

// ServeStaticFiles configures HTTP handler that serves static files and dynamically patches index.html to embed CSRF token, etc.
func (s *Server) ServeStaticFiles(m *mux.Router, fs http.FileSystem) {
	h := http.FileServer(fs)

	// read bytes from 'index.html'.
	indexBytes := maybeReadIndexBytes(fs)

	m.PathPrefix("/").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.isKnownUIRoute(r.URL.Path) {
			r2 := new(http.Request)
			*r2 = *r
			r2.URL = new(url.URL)
			*r2.URL = *r.URL
			r2.URL.Path = "/"
			r = r2
		}

		rc := s.captureRequestContext(w, r)

		if !isAuthenticated(rc) {
			return
		}

		if !requireUIUser(rc.req.Context(), rc) {
			http.Error(w, `UI Access denied. See https://github.com/kopia/kopia/issues/880#issuecomment-798421751 for more information.`, http.StatusForbidden)
			return
		}

		if r.URL.Path == "/" && indexBytes != nil {
			var sessionID string

			if cookie, err := r.Cookie(kopiaSessionCookie); err == nil {
				// already in a session, likely a new tab was opened
				sessionID = cookie.Value
			} else {
				sessionID = uuid.NewString()

				http.SetCookie(w, &http.Cookie{
					Name:  kopiaSessionCookie,
					Value: sessionID,
					Path:  "/",
				})
			}

			http.ServeContent(w, r, "/", clock.Now(), bytes.NewReader(s.patchIndexBytes(sessionID, indexBytes)))
			return
		}

		h.ServeHTTP(w, r)
	})
}

// Options encompasses all API server options.
type Options struct {
	ConfigFile             string
	ConnectOptions         *repo.ConnectOptions
	RefreshInterval        time.Duration
	MaxConcurrency         int
	Authenticator          auth.Authenticator
	Authorizer             auth.Authorizer
	PasswordPersist        passwordpersist.Strategy
	AuthCookieSigningKey   string
	LogRequests            bool
	UIUser                 string // name of the user allowed to access the UI API
	UIPreferencesFile      string // name of the JSON file storing UI preferences
	ServerControlUser      string // name of the user allowed to access the server control API
	DisableCSRFTokenChecks bool
	UITitlePrefix          string
}

// InitRepositoryFunc is a function that attempts to connect to/open repository.
type InitRepositoryFunc func(ctx context.Context) (repo.Repository, error)

func (s *Server) setInitRepositoryTaskID(taskID string) {
	s.initTaskMutex.Lock()
	defer s.initTaskMutex.Unlock()

	s.initRepositoryTaskID = taskID
}

func (s *Server) getInitRepositoryTaskID() string {
	s.initTaskMutex.Lock()
	defer s.initTaskMutex.Unlock()

	return s.initRepositoryTaskID
}

// InitRepositoryAsync starts a task that initializes the repository by invoking the provided callback
// and initializes the repository when done. The initializer may return nil to indicate there
// is no repository configured.
func (s *Server) InitRepositoryAsync(ctx context.Context, mode string, initializer InitRepositoryFunc, wait bool) (string, error) {
	var wg sync.WaitGroup

	var taskID string

	wg.Add(1)

	// nolint:errcheck
	go s.taskmgr.Run(ctx, "Repository", mode, func(ctx context.Context, ctrl uitask.Controller) error {
		// we're still holding a lock, until wg.Done(), so no lock around this is needed.
		taskID = ctrl.CurrentTaskID()
		s.setInitRepositoryTaskID(taskID)
		wg.Done()

		defer s.setInitRepositoryTaskID("")

		cctx, cancel := context.WithCancel(ctx)
		defer cancel()

		ctrl.OnCancel(func() {
			cancel()
		})

		// run initializer in cancelable context.
		rep, err := initializer(cctx)

		if cctx.Err() != nil {
			// context canceled
			return errors.Errorf("operation has been canceled")
		}

		if err != nil {
			return errors.Wrap(err, "error opening repository")
		}

		if rep == nil {
			log(ctx).Infof("Repository not configured.")
		}

		if err = s.SetRepository(ctx, rep); err != nil {
			return errors.Wrap(err, "error connecting to repository")
		}

		return nil
	})

	wg.Wait()

	if wait {
		if ti, ok := s.taskmgr.WaitForTask(ctx, taskID, -1); ok {
			return taskID, ti.Error
		}
	}

	return taskID, nil
}

// RetryInitRepository wraps provided initialization function with retries until the context gets canceled.
func RetryInitRepository(initialize InitRepositoryFunc) InitRepositoryFunc {
	return func(ctx context.Context) (repo.Repository, error) {
		nextSleepTime := retryInitRepositorySleepOnError

		// async connection - keep trying to open repository until context gets canceled.
		for {
			if cerr := ctx.Err(); cerr != nil {
				// context canceled, bail
				// nolint:wrapcheck
				return nil, cerr
			}

			rep, rerr := initialize(ctx)
			if rerr == nil {
				return rep, nil
			}

			log(ctx).Warnf("unable to open repository: %v, will keep trying until canceled. Sleeping for %v", rerr, nextSleepTime)

			if !clock.SleepInterruptibly(ctx, nextSleepTime) {
				// nolint:wrapcheck
				return nil, ctx.Err()
			}

			nextSleepTime *= 2
			if nextSleepTime > maxRetryInitRepositorySleepOnError {
				nextSleepTime = maxRetryInitRepositorySleepOnError
			}
		}
	}
}

func (s *Server) getOrCreateSourceManager(ctx context.Context, src snapshot.SourceInfo) *sourceManager {
	s.serverMutex.Lock()
	defer s.serverMutex.Unlock()

	if s.sourceManagers[src] == nil {
		log(ctx).Debugf("creating source manager for %v", src)
		sm := newSourceManager(src, s, s.rep)
		s.sourceManagers[src] = sm

		sm.refreshStatus(ctx)
		sm.start(ctx, s.rep)
	}

	return s.sourceManagers[src]
}

func (s *Server) deleteSourceManager(ctx context.Context, src snapshot.SourceInfo) bool {
	s.serverMutex.Lock()
	sm := s.sourceManagers[src]
	delete(s.sourceManagers, src)
	s.serverMutex.Unlock()

	if sm == nil {
		return false
	}

	sm.stop(ctx)
	sm.waitUntilStopped(ctx)

	return true
}

func (s *Server) allSourceManagers() map[snapshot.SourceInfo]*sourceManager {
	s.serverMutex.Lock()
	defer s.serverMutex.Unlock()

	result := map[snapshot.SourceInfo]*sourceManager{}

	for k, v := range s.sourceManagers {
		result[k] = v
	}

	return result
}

// New creates a Server.
// The server will manage sources for a given username@hostname.
func New(ctx context.Context, options *Options) (*Server, error) {
	if options.Authorizer == nil {
		return nil, errors.Errorf("missing authorizer")
	}

	if options.PasswordPersist == nil {
		return nil, errors.Errorf("missing password persistence")
	}

	if options.AuthCookieSigningKey == "" {
		// generate random signing key
		options.AuthCookieSigningKey = uuid.New().String()
		log(ctx).Debugf("generated random auth cookie signing key: %v", options.AuthCookieSigningKey)
	}

	s := &Server{
		options:              *options,
		sourceManagers:       map[snapshot.SourceInfo]*sourceManager{},
		uploadSemaphore:      semaphore.NewWeighted(1),
		grpcServerState:      makeGRPCServerState(options.MaxConcurrency),
		authenticator:        options.Authenticator,
		authorizer:           options.Authorizer,
		taskmgr:              uitask.NewManager(),
		mounts:               map[object.ID]mount.Controller{},
		authCookieSigningKey: []byte(options.AuthCookieSigningKey),
	}

	return s, nil
}
