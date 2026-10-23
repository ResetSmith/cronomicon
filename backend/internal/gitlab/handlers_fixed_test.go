package gitlab

// NewHandlers creates handler wrappers around one fixed Service. The server
// asks the registry (NewRegistryHandlers); the tests that drive one Service's
// routes build them this way (2.4.2: moved here, nothing else called it).
func NewHandlers(svc *Service) *Handlers {
	return &Handlers{svc: svc}
}
