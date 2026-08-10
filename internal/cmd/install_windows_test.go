//go:build windows

package cmd

import (
	"reflect"
	"testing"
)

func TestNativeInstallPersistsExplicitTransport(t *testing.T) {
	exe := `C:\Program Files\VIIPER\viiper.exe`
	logFile := `C:\Users\test user\AppData\Local\VIIPER\viiper.log`

	wantArgs := []string{"server", "--transport", "native-ude", "--log.file", logFile}
	if got := serverArguments("native-ude", logFile); !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("server arguments=%q want=%q", got, wantArgs)
	}

	wantCommand := `"C:\Program Files\VIIPER\viiper.exe" server --transport native-ude --log.file "C:\Users\test user\AppData\Local\VIIPER\viiper.log"`
	if got := windowsAutorunCommand(exe, "native-ude", logFile); got != wantCommand {
		t.Fatalf("autorun command=%q want=%q", got, wantCommand)
	}
}
