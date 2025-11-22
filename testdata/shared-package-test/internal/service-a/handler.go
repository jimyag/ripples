package servicea

import "example.com/shared-package-test/pkg/common"

// Handler handles requests for service A
type Handler struct {
	logger *common.Logger
}

// NewHandler creates a new handler
func NewHandler() *Handler {
	return &Handler{
		logger: common.NewLogger("ServiceA"),
	}
}

// ProcessRequest processes a request
func (h *Handler) ProcessRequest(req string) {
	h.logger.Log("Processing request: " + req)
	// Do some processing
	h.logger.LogWithLevel("INFO", "Request processed successfully")
	// Also use standalone function
	common.LogMessage("ServiceA: " + req)
}

// Server implements the Runner interface
type Server struct {
	handler *Handler
}

// NewServer creates a new server
func NewServer() *Server {
	return &Server{
		handler: NewHandler(),
	}
}

// Run implements Runner.Run - this is called by pkg/common.RunServer
func (s *Server) Run() error {
	// This is the critical function - when modified, should only affect service-a
	s.internalServiceLogic()
	return nil
}

// internalServiceLogic is service-a specific logic
func (s *Server) internalServiceLogic() {
	// Service A specific implementation
	s.handler.ProcessRequest("internal-task")
}
