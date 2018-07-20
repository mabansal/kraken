package tagserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"time"

	"code.uber.internal/infra/kraken/build-index/tagclient"
	"code.uber.internal/infra/kraken/build-index/tagstore"
	"code.uber.internal/infra/kraken/build-index/tagtype"
	"code.uber.internal/infra/kraken/core"
	"code.uber.internal/infra/kraken/lib/backend"
	"code.uber.internal/infra/kraken/lib/backend/backenderrors"
	"code.uber.internal/infra/kraken/lib/hostlist"
	"code.uber.internal/infra/kraken/lib/middleware"
	"code.uber.internal/infra/kraken/lib/persistedretry"
	"code.uber.internal/infra/kraken/lib/persistedretry/tagreplication"
	"code.uber.internal/infra/kraken/origin/blobclient"
	"code.uber.internal/infra/kraken/utils/handler"
	"code.uber.internal/infra/kraken/utils/httputil"
	"code.uber.internal/infra/kraken/utils/listener"
	"code.uber.internal/infra/kraken/utils/log"

	"github.com/pressly/chi"
	chimiddleware "github.com/pressly/chi/middleware"
	"github.com/uber-go/tally"
)

// Server provides tag operations for the build-index.
type Server struct {
	config            Config
	stats             tally.Scope
	backends          *backend.Manager
	localOriginDNS    string
	localOriginClient blobclient.ClusterClient
	localReplicas     *hostlist.List
	store             tagstore.Store

	// For async new tag replication.
	remotes               tagreplication.Remotes
	tagReplicationManager persistedretry.Manager
	provider              tagclient.Provider

	// For checking if a tag has all dependent blobs.
	tagTypes tagtype.Manager
}

// New creates a new Server.
func New(
	config Config,
	stats tally.Scope,
	backends *backend.Manager,
	localOriginDNS string,
	localOriginClient blobclient.ClusterClient,
	localReplicas *hostlist.List,
	store tagstore.Store,
	remotes tagreplication.Remotes,
	tagReplicationManager persistedretry.Manager,
	provider tagclient.Provider,
	tagTypes tagtype.Manager) *Server {

	config = config.applyDefaults()

	stats = stats.Tagged(map[string]string{
		"module": "tagserver",
	})

	return &Server{
		config:                config,
		stats:                 stats,
		backends:              backends,
		localOriginDNS:        localOriginDNS,
		localOriginClient:     localOriginClient,
		localReplicas:         localReplicas,
		store:                 store,
		remotes:               remotes,
		tagReplicationManager: tagReplicationManager,
		provider:              provider,
		tagTypes:              tagTypes,
	}
}

// Handler returns an http.Handler for s.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.StatusCounter(s.stats))
	r.Use(middleware.LatencyTimer(s.stats))

	r.Get("/health", handler.Wrap(s.healthHandler))

	r.Put("/tags/:tag/digest/:digest", handler.Wrap(s.putTagHandler))
	r.Head("/tags/:tag", handler.Wrap(s.hasTagHandler))
	r.Get("/tags/:tag", handler.Wrap(s.getTagHandler))

	r.Get("/repositories/:repo/tags", handler.Wrap(s.listRepositoryHandler))

	r.Get("/list/*", handler.Wrap(s.listHandler))

	r.Post("/remotes/tags/:tag", handler.Wrap(s.replicateTagHandler))

	r.Get("/origin", handler.Wrap(s.getOriginHandler))

	r.Post(
		"/internal/duplicate/remotes/tags/:tag/digest/:digest",
		handler.Wrap(s.duplicateReplicateTagHandler))

	r.Put(
		"/internal/duplicate/tags/:tag/digest/:digest",
		handler.Wrap(s.duplicatePutTagHandler))

	r.Mount("/debug", chimiddleware.Profiler())

	return r
}

// ListenAndServe is a blocking call which runs s.
func (s *Server) ListenAndServe() error {
	log.Infof("Starting tag server on %s", s.config.Listener)
	return listener.Serve(s.config.Listener, s.Handler())
}

func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) error {
	fmt.Fprintln(w, "OK")
	return nil
}

func (s *Server) putTagHandler(w http.ResponseWriter, r *http.Request) error {
	tag, err := httputil.ParseParam(r, "tag")
	if err != nil {
		return err
	}
	d, err := httputil.ParseDigest(r, "digest")
	if err != nil {
		return err
	}
	replicate, err := strconv.ParseBool(httputil.GetQueryArg(r, "replicate", "false"))
	if err != nil {
		return handler.Errorf("parse query arg `replicate`: %s", err)
	}

	deps, err := s.getDeps(tag, d)
	if err != nil {
		return err
	}
	if err := s.putTag(tag, d, deps); err != nil {
		return err
	}

	if replicate {
		if err := s.replicateTag(tag, d, deps); err != nil {
			return err
		}
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) duplicatePutTagHandler(w http.ResponseWriter, r *http.Request) error {
	tag, err := httputil.ParseParam(r, "tag")
	if err != nil {
		return err
	}
	d, err := httputil.ParseDigest(r, "digest")
	if err != nil {
		return err
	}

	var req tagclient.DuplicatePutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return handler.Errorf("decode body: %s", err)
	}
	delay := req.Delay

	if err := s.store.Put(tag, d, delay); err != nil {
		return handler.Errorf("storage: %s", err)
	}

	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) getTagHandler(w http.ResponseWriter, r *http.Request) error {
	tag, err := httputil.ParseParam(r, "tag")
	if err != nil {
		return err
	}

	d, err := s.store.Get(tag)
	if err != nil {
		if err == tagstore.ErrTagNotFound {
			return handler.ErrorStatus(http.StatusNotFound)
		}
		return handler.Errorf("storage: %s", err)
	}

	if _, err := io.WriteString(w, d.String()); err != nil {
		return handler.Errorf("write digest: %s", err)
	}
	return nil
}

func (s *Server) hasTagHandler(w http.ResponseWriter, r *http.Request) error {
	tag, err := httputil.ParseParam(r, "tag")
	if err != nil {
		return err
	}

	client, err := s.backends.GetClient(tag)
	if err != nil {
		return handler.Errorf("backend manager: %s", err)
	}
	if _, err := client.Stat(tag); err != nil {
		if err == backenderrors.ErrBlobNotFound {
			return handler.ErrorStatus(http.StatusNotFound)
		}
		return err
	}
	return nil
}

func (s *Server) listHandler(w http.ResponseWriter, r *http.Request) error {
	prefix := r.URL.Path[len("/list/"):]

	client, err := s.backends.GetClient(prefix)
	if err != nil {
		return handler.Errorf("backend manager: %s", err)
	}
	names, err := client.List(prefix)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(w).Encode(&names); err != nil {
		return handler.Errorf("json encode: %s", err)
	}
	return nil
}

// TODO(codyg): Remove this.
func (s *Server) listRepositoryHandler(w http.ResponseWriter, r *http.Request) error {
	repo, err := httputil.ParseParam(r, "repo")
	if err != nil {
		return err
	}

	client, err := s.backends.GetClient(repo)
	if err != nil {
		return handler.Errorf("backend manager: %s", err)
	}
	tags, err := client.List(path.Join(repo, "_manifests/tags"))
	if err != nil {
		return err
	}
	if err := json.NewEncoder(w).Encode(&tags); err != nil {
		return handler.Errorf("json encode: %s", err)
	}
	return nil
}

func (s *Server) replicateTagHandler(w http.ResponseWriter, r *http.Request) error {
	tag, err := httputil.ParseParam(r, "tag")
	if err != nil {
		return err
	}

	d, err := s.store.Get(tag)
	if err != nil {
		if err == tagstore.ErrTagNotFound {
			return handler.ErrorStatus(http.StatusNotFound)
		}
		return handler.Errorf("storage: %s", err)
	}
	deps, err := s.getDeps(tag, d)
	if err != nil {
		return err
	}
	if err := s.replicateTag(tag, d, deps); err != nil {
		return err
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) duplicateReplicateTagHandler(w http.ResponseWriter, r *http.Request) error {
	tag, err := httputil.ParseParam(r, "tag")
	if err != nil {
		return err
	}
	d, err := httputil.ParseDigest(r, "digest")
	if err != nil {
		return handler.Errorf("get dependency resolver: %s", err)
	}
	var req tagclient.DuplicateReplicateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return handler.Errorf("decode body: %s", err)
	}

	destinations := s.remotes.Match(tag)

	for _, dest := range destinations {
		task := tagreplication.NewTaskWithDelay(tag, d, req.Dependencies, dest, req.Delay)
		if err := s.tagReplicationManager.Add(task); err != nil {
			return handler.Errorf("add replicate task: %s", err)
		}
	}

	return nil
}

func (s *Server) getOriginHandler(w http.ResponseWriter, r *http.Request) error {
	if _, err := io.WriteString(w, s.localOriginDNS); err != nil {
		return handler.Errorf("write local origin dns: %s", err)
	}
	return nil
}

func (s *Server) getDeps(tag string, d core.Digest) (core.DigestList, error) {
	depResolver, err := s.tagTypes.GetDependencyResolver(tag)
	if err != nil {
		return nil, handler.Errorf("get dependency resolver: %s", err)
	}
	deps, err := depResolver.Resolve(tag, d)
	if err != nil {
		return nil, handler.Errorf("get dependencies: %s", err)
	}
	return deps, nil
}

func (s *Server) putTag(tag string, d core.Digest, deps core.DigestList) error {
	for _, dep := range deps {
		if _, err := s.localOriginClient.Stat(tag, dep); err == blobclient.ErrBlobNotFound {
			return handler.Errorf("cannot upload tag, missing dependency %s", dep)
		} else if err != nil {
			return handler.Errorf("check blob: %s", err)
		}
	}

	if err := s.store.Put(tag, d, 0); err != nil {
		return handler.Errorf("storage: %s", err)
	}

	localReplicas, err := s.localReplicas.Resolve()
	if err != nil {
		return fmt.Errorf("hostlist: %s", err)
	}

	var delay time.Duration
	var successes int
	for addr := range localReplicas {
		delay += s.config.DuplicatePutStagger
		client := s.provider.Provide(addr)
		if err := client.DuplicatePut(tag, d, delay); err != nil {
			log.Errorf("Error duplicating put task to %s: %s", addr, err)
		} else {
			successes++
		}
	}
	if len(localReplicas) != 0 && successes == 0 {
		s.stats.Counter("duplicate_put_failures").Inc(1)
	}
	return nil
}

func (s *Server) replicateTag(tag string, d core.Digest, deps core.DigestList) error {
	destinations := s.remotes.Match(tag)
	if len(destinations) == 0 {
		return nil
	}

	for _, dest := range destinations {
		task := tagreplication.NewTask(tag, d, deps, dest)
		if err := s.tagReplicationManager.Add(task); err != nil {
			return handler.Errorf("add replicate task: %s", err)
		}
	}

	localReplicas, err := s.localReplicas.Resolve()
	if err != nil {
		return fmt.Errorf("hostlist: %s", err)
	}

	var delay time.Duration
	var successes int
	for addr := range localReplicas { // Loops in random order.
		delay += s.config.DuplicateReplicateStagger
		client := s.provider.Provide(addr)
		if err := client.DuplicateReplicate(tag, d, deps, delay); err != nil {
			log.Errorf("Error duplicating replicate task to %s: %s", addr, err)
		} else {
			successes++
		}
	}
	if len(localReplicas) != 0 && successes == 0 {
		s.stats.Counter("duplicate_replicate_failures").Inc(1)
	}
	return nil
}