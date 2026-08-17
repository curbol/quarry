package browse

import (
	"os/exec"
	"runtime"
)

// openBrowser best-effort opens the default browser at url. Failure is ignored: the
// caller has already printed the URL for the user to open manually.
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	_ = exec.Command(cmd, append(args, url)...).Start()
}
