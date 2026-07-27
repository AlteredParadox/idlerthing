package web

import (
	"net/http"
)

// sourceBaseURL is where the Corresponding Source is published. AGPL §13
// requires a network user be offered the source of the version they are
// interacting with — a downstream fork that changes this (and rebuilds) makes
// /source point at ITS source, which is exactly the intent.
const sourceBaseURL = "https://github.com/AlteredParadox/idlerthing"

// SetLegal injects the embedded license texts. They live at the repo root
// (see notices.go) and go:embed cannot reach a parent directory, so package
// main hands them over rather than this package embedding its own copies.
func (s *Server) SetLegal(license, thirdParty []byte) {
	s.license = license
	s.thirdParty = thirdParty
}

// SetVersion records the build's version so /source can pin its offer to the
// exact revision this binary was built from.
func (s *Server) SetVersion(v string) { s.version = v }

// Version returns the build version ("dev" for unstamped builds).
func (s *Server) Version() string {
	if s.version == "" {
		return "dev"
	}
	return s.version
}

// sourceURL is the AGPL §13 offer target. A stamped release pins to its tag,
// so the source you are offered is the source that is running; an unstamped
// build can only point at the repository.
func (s *Server) sourceURL() string {
	if v := s.Version(); v != "dev" {
		return sourceBaseURL + "/tree/" + v
	}
	return sourceBaseURL
}

// serveLegalText serves an embedded text blob as plain UTF-8. These are
// deliberately UNAUTHENTICATED: AGPL §13 obliges the offer to every network
// user, and a login wall would defeat it.
//
// The body is read through a closure at REQUEST time, so route registration
// does not have to happen after SetLegal.
func (s *Server) serveLegalText(get func() []byte, missing string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := get()
		if len(body) == 0 {
			http.Error(w, missing, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// Tied to the binary, so it only changes when the build does.
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write(body)
	}
}

// handleSource implements the AGPL §13 source offer.
func (s *Server) handleSource(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, s.sourceURL(), http.StatusFound)
}
