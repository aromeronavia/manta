package main

import (
	"embed"
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

//go:embed static
var staticFiles embed.FS

var matchIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

// Server is the web front: a landing page that takes a match id, the job API
// and the per-match viewers.
type Server struct {
	mux *http.ServeMux
	mgr *Manager
}

func NewServer(mgr *Manager) *Server {
	s := &Server{mux: http.NewServeMux(), mgr: mgr}
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	s.mux.HandleFunc("GET /api/matches", s.handleList)
	s.mux.HandleFunc("POST /api/matches/{id}", s.handleStart)
	s.mux.HandleFunc("GET /api/jobs/{id}", s.handleJob)
	s.mux.HandleFunc("GET /match/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
	})
	s.mux.HandleFunc("/match/{id}/", s.handleViewer)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	page, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(page)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	matches, err := s.mgr.opts.Store.List()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if matches == nil {
		matches = []MatchSummary{}
	}
	jobs := s.mgr.Active()
	if jobs == nil {
		jobs = []*Job{}
	}
	writeJSON(w, map[string]interface{}{"matches": matches, "jobs": jobs})
}

func parseMatchID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.PathValue("id"))
	if !matchIDPattern.MatchString(raw) {
		httpError(w, http.StatusBadRequest, "match id must be a positive number")
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		httpError(w, http.StatusBadRequest, "match id must be a positive number")
		return 0, false
	}
	return id, true
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	id, ok := parseMatchID(w, r)
	if !ok {
		return
	}
	job := s.mgr.Start(id)
	if job.State != JobReady {
		w.WriteHeader(http.StatusAccepted)
	}
	writeJSON(w, job)
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	id, ok := parseMatchID(w, r)
	if !ok {
		return
	}
	job, ok := s.mgr.Get(id)
	if !ok {
		httpError(w, http.StatusNotFound, "no such job")
		return
	}
	writeJSON(w, job)
}

func (s *Server) handleViewer(w http.ResponseWriter, r *http.Request) {
	id, ok := parseMatchID(w, r)
	if !ok {
		return
	}
	if !s.mgr.opts.Store.Has(id) {
		http.Redirect(w, r, "/?match="+strconv.FormatInt(id, 10), http.StatusFound)
		return
	}
	h, err := s.mgr.Handler(id)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.StripPrefix("/match/"+strconv.FormatInt(id, 10), h).ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
