package registry_test

import (
	"testing"

	_ "github.com/Alia5/VIIPER/internal/devicecatalog"
	"github.com/Alia5/VIIPER/internal/server/api"
)

// The Xbox One/Series persona has a typed, explicit retained factory. Loading
// the generic device catalog must still not make it creatable through the
// permissive deviceSpecific registry path.
func TestXboxOnePersonaRemainsUnregistered(t *testing.T) {
	if registration := api.GetRegistration("xbox360"); registration == nil {
		t.Fatal("canonical registry positive control xbox360 is not registered")
	}
	for _, name := range []string{"xboxone", "xbox-one", "xboxseries", "xbox-series"} {
		if registration := api.GetRegistration(name); registration != nil {
			t.Fatalf("blocked persona %q unexpectedly registered as %T", name, registration)
		}
	}
}
