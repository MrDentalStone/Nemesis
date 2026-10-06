//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/MrDentalStone/Nemesis/internal/setup"
)

const (
	createNoWindow  = 0x08000000
	detachedProcess = 0x00000008
)

type winSys struct{}

func folders() (setup.Folders, error) {
	var f setup.Folders
	var err error
	get := func(id *windows.KNOWNFOLDERID) string {
		if err != nil {
			return ""
		}
		var p string
		p, err = windows.KnownFolderPath(id, 0)
		return p
	}
	f.Home, _ = os.UserHomeDir()
	f.Roaming = get(windows.FOLDERID_RoamingAppData)
	f.Local = get(windows.FOLDERID_LocalAppData)
	f.Documents = get(windows.FOLDERID_Documents)
	f.Programs = get(windows.FOLDERID_Programs)
	f.Desktop = get(windows.FOLDERID_Desktop)
	return f, err
}

func hidden(c *exec.Cmd) *exec.Cmd {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return c
}

func (winSys) Run(name string, args ...string) (string, error) {
	out, err := hidden(exec.Command(name, args...)).CombinedOutput()
	return string(out), err
}

// Shortcut makes a .lnk through WScript.Shell; the values travel in the
// environment so no quoting can break the script.
func (winSys) Shortcut(lnk, target, args, workDir, desc string) error {
	script := `$s = (New-Object -ComObject WScript.Shell).CreateShortcut($env:NS_LNK); ` +
		`$s.TargetPath = $env:NS_TARGET; $s.Arguments = $env:NS_ARGS; $s.WorkingDirectory = $env:NS_WD; ` +
		`$s.Description = $env:NS_DESC; $s.Save()`
	c := hidden(exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script))
	c.Env = append(os.Environ(), "NS_LNK="+lnk, "NS_TARGET="+target, "NS_ARGS="+args, "NS_WD="+workDir, "NS_DESC="+desc)
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (winSys) SetUninstallEntry(key string, str map[string]string, dword map[string]uint32) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, key, registry.ALL_ACCESS)
	if err != nil {
		return err
	}
	defer k.Close()
	for n, v := range str {
		if err := k.SetStringValue(n, v); err != nil {
			return err
		}
	}
	for n, v := range dword {
		if err := k.SetDWordValue(n, v); err != nil {
			return err
		}
	}
	return nil
}

func (winSys) DeleteUninstallEntry(key string) error {
	err := registry.DeleteKey(registry.CURRENT_USER, key)
	if err == registry.ErrNotExist {
		return nil
	}
	return err
}

func openBrowser(url string) error {
	return hidden(exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url)).Start()
}

func messageBox(title, text string, warn bool) {
	style := uint32(windows.MB_OK | windows.MB_ICONINFORMATION | windows.MB_SETFOREGROUND)
	if warn {
		style = windows.MB_OK | windows.MB_ICONWARNING | windows.MB_SETFOREGROUND
	}
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString(title)
	windows.MessageBox(0, t, c, style)
}

// Processes lists running programs named nemesis* with their full path.
func (winSys) Processes() ([]setup.Proc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var out []setup.Proc
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		name := strings.ToLower(windows.UTF16ToString(e.ExeFile[:]))
		if !strings.HasPrefix(name, "nemesis") {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, e.ProcessID)
		if err != nil {
			continue // another user's process, or already gone
		}
		buf := make([]uint16, windows.MAX_LONG_PATH)
		n := uint32(len(buf))
		if windows.QueryFullProcessImageName(h, 0, &buf[0], &n) == nil {
			out = append(out, setup.Proc{PID: e.ProcessID, Path: windows.UTF16ToString(buf[:n])})
		}
		windows.CloseHandle(h)
	}
	return out, nil
}

// Kill ends a process and waits up to 5 s for it to exit, so its exe can be
// replaced right after.
func (winSys) Kill(pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return err
	}
	if ev, err := windows.WaitForSingleObject(h, 5000); err != nil || ev != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("it did not exit in time")
	}
	return nil
}

// removeLater deletes dir a few seconds after this process exits: an exe
// cannot delete the folder it runs from.
func removeLater(dir, cwd string) error {
	c := exec.Command("cmd.exe")
	c.Dir = cwd
	c.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       fmt.Sprintf(`cmd.exe /c ping -n 4 127.0.0.1 >nul & rmdir /s /q "%s"`, dir),
		HideWindow:    true,
		CreationFlags: createNoWindow | detachedProcess,
	}
	return c.Start()
}
