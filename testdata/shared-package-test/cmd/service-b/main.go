package main

import (
	"example.com/shared-package-test/pkg/common"
	serviceb "example.com/shared-package-test/internal/service-b"
)

func main() {
	// Direct call to handler
	handler := serviceb.NewHandler()
	handler.ProcessRequest("test-request-b")

	// Also use the shared runner pattern
	server := serviceb.NewServer()
	common.RunServer(server)
}
