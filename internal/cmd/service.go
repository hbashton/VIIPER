package cmd

// ServiceCommand hosts the native UDE broker under the Windows Service
// Control Manager. It is intentionally a distinct command from Server so an
// interactive VIIPER instance can never accidentally claim the privileged
// native driver session.
type ServiceCommand struct {
	Server `embed:""`
}
