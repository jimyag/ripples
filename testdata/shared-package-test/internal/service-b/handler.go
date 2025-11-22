package serviceb

import "example.com/shared-package-test/pkg/common"

// Handler handles requests for service B
type Handler struct {
	logger *common.Logger
}

// NewHandler creates a new handler
func NewHandler() *Handler {
	return &Handler{
		logger: common.NewLogger("ServiceB"),
	}
}

// ProcessRequest processes a request
func (h *Handler) ProcessRequest(req string) {
	h.logger.LogWithLevel("DEBUG", "Starting to process: "+req)
	// Do some different processing
	h.logger.Log("Request handled by service B")
	// Also use standalone function
	common.LogMessage("ServiceB: " + req)
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
	// This is service-b specific logic
	s.internalServiceLogic()
	return nil
}

// internalServiceLogic is service-b specific logic
func (s *Server) internalServiceLogic() {
	// Service B specific implementation
	s.handler.ProcessRequest("internal-task")
}
