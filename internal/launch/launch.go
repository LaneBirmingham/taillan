// Package launch opens TailLAN's window and folders using whatever the
// OS already has: a Chromium-family browser in "app" mode (a plain
// window with no tabs or address bar) when one is installed, else the
// default browser.
package launch

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// OpenApp opens url in an app-style window if possible.
func OpenApp(url string) error {
	if bin := findChromium(); bin != "" {
		cmd := exec.Command(bin, "--app="+url, "--window-size=1060,800", "--new-window")
		if err := cmd.Start(); err == nil {
			go cmd.Wait()
			return nil
		}
	}
	return OpenBrowser(url)
}

// OpenBrowser opens url in the default browser.
func OpenBrowser(url string) error {
	return open(url)
}

// OpenFolder shows dir in the file manager, creating it if needed.
func OpenFolder(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return open(dir)
}

func open(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case "darwin":
		cmd = exec.Command("open", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func findChromium() string {
	if os.Getenv("TAILLAN_BROWSER") == "default" {
		return ""
	}
	if b := os.Getenv("TAILLAN_BROWSER"); b != "" {
		return b
	}
	switch runtime.GOOS {
	case "windows":
		var cands []string
		for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
			base := os.Getenv(env)
			if base == "" {
				continue
			}
			cands = append(cands,
				filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`),
				filepath.Join(base, `Google\Chrome\Application\chrome.exe`),
				filepath.Join(base, `BraveSoftware\Brave-Browser\Application\brave.exe`),
			)
		}
		return firstExisting(cands)
	case "darwin":
		return firstExisting([]string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		})
	default:
		for _, n := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "brave", "brave-browser", "microsoft-edge", "microsoft-edge-stable"} {
			if p, err := exec.LookPath(n); err == nil {
				return p
			}
		}
	}
	return ""
}

func firstExisting(paths []string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
