package main

import (
	"example.com/shared-package-test/pkg/common"
	servicea "example.com/shared-package-test/internal/service-a"
)

func main() {
	// Direct call to handler (for TestInternalPackageNotCrossingShared)
	handler := servicea.NewHandler()
	handler.ProcessRequest("test-request-a")

	// Also use the shared runner pattern (for TestInternalViaSharedPackage)
	server := servicea.NewServer()
	common.RunServer(server)
}
