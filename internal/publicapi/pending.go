package publicapi

import "net/http"

func (s *Server) pending(w http.ResponseWriter, _ *http.Request) {
	requestID := NewRequestID()
	writeError(w, requestID, invalid(requestID, "", "this endpoint is not served yet"))
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request)          { s.pending(w, r) }
func (s *Server) handleModel(w http.ResponseWriter, r *http.Request)           { s.pending(w, r) }
func (s *Server) handlePowerLevels(w http.ResponseWriter, r *http.Request)     { s.pending(w, r) }
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request)       { s.pending(w, r) }
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) { s.pending(w, r) }
func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request)      { s.pending(w, r) }
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request)          { s.pending(w, r) }
func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request)       { s.pending(w, r) }
func (s *Server) handleGeneration(w http.ResponseWriter, r *http.Request)      { s.pending(w, r) }
func (s *Server) handleGenerationByKey(w http.ResponseWriter, r *http.Request) { s.pending(w, r) }
