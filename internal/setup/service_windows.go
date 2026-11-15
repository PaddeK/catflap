package setup

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/PaddeK/catflap/internal/homelab"
	"github.com/PaddeK/catflap/internal/osutil"
)

// taskName names the homelab's scheduled task.
const taskName = "catflap-homelab"

// The task starts at boot, without anyone logged in, as the user (S4U: no
// password stored). Admin accounts run it elevated, so it can manage
// administrators_authorized_keys. MultipleInstancesPolicy Queue lets
// `catflap restart` queue the next run before stopping the current one,
// so the restart completes even when it cuts the session that asked.
const taskXML = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>catflap homelab: lets your enrolled devices reach this computer. Remove it with: catflap uninstall</Description>
    <SecurityDescriptor>%s</SecurityDescriptor>
  </RegistrationInfo>
  <Triggers>
    <BootTrigger>
      <Enabled>true</Enabled>
    </BootTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>%s</UserId>
      <LogonType>S4U</LogonType>
      <RunLevel>%s</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>Queue</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>5</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
      <Arguments>homelab run --service</Arguments>
      <WorkingDirectory>%s</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`

// taskService is the homelab as a scheduled task.
type taskService struct{ p homelab.Paths }

func platformService(p homelab.Paths) (homelabService, error) {
	return taskService{p}, nil
}

// binPath is where install.ps1 puts catflap, too.
func (taskService) binPath() (string, error) {
	local, err := os.UserCacheDir() // %LOCALAPPDATA%
	if err != nil {
		return "", err
	}
	return filepath.Join(local, "Programs", "catflap", "catflap.exe"), nil
}

func (taskService) installed() bool {
	return exec.Command("schtasks", "/Query", "/TN", taskName).Run() == nil
}

func (s taskService) install(bin string, out io.Writer) error {
	sid, err := osutil.UserSID()
	if err != nil {
		return err
	}
	level := "LeastPrivilege"
	if osutil.IsAdmin() {
		level = "HighestAvailable"
		fmt.Fprintln(out, "    your account is an administrator: the homelab runs elevated, so it can add\n    device SSH keys to administrators_authorized_keys (where Windows' SSH server reads them)")
	}
	// SYSTEM and Administrators may do anything with the task; the user
	// may read and run it (catflap restart), not change what it runs.
	sddl := fmt.Sprintf("D:(A;;FA;;;SY)(A;;FA;;;BA)(A;;GRGX;;;%s)", sid)
	dir := filepath.Dir(s.p.State)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	def := fmt.Sprintf(taskXML, sddl, sid, level, xmlEscape(bin), xmlEscape(dir))
	xmlPath := filepath.Join(dir, "catflap-homelab-task.xml")
	if err := os.WriteFile(xmlPath, utf16File(def), 0o600); err != nil {
		return err
	}
	defer os.Remove(xmlPath)
	fmt.Fprintf(out, "==> registering the scheduled task %s (starts at boot)\n", taskName)
	return asAdmin(out, "register the homelab's scheduled task",
		[]string{"schtasks", "/Create", "/TN", taskName, "/XML", xmlPath, "/F"},
		[]string{"schtasks", "/Run", "/TN", taskName})
}

func (s taskService) start(out io.Writer) error {
	if exec.Command("schtasks", "/Run", "/TN", taskName).Run() == nil {
		return nil
	}
	return asAdmin(out, "start the homelab's scheduled task", []string{"schtasks", "/Run", "/TN", taskName})
}

func (s taskService) stop(out io.Writer) error {
	return shutdownHomelab(s.p, 15*time.Second)
}

func (s taskService) restart(out io.Writer, noBlock bool) error {
	if !homelabRunning(s.p) {
		return s.start(out)
	}
	// Queue the next run first (it starts as soon as this one ends), then
	// stop this one. Once queued, the restart doesn't need this session.
	if err := s.start(out); err != nil {
		return err
	}
	if noBlock {
		return homelab.Control(s.p).Do(context.Background(), "POST", "/shutdown", nil, nil)
	}
	return shutdownHomelab(s.p, 15*time.Second)
}

func (s taskService) remove(out io.Writer) error {
	if err := s.stop(out); err != nil {
		return err
	}
	if exec.Command("schtasks", "/Delete", "/TN", taskName, "/F").Run() != nil {
		if err := asAdmin(out, "remove the homelab's scheduled task", []string{"schtasks", "/Delete", "/TN", taskName, "/F"}); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "    stopped and removed the scheduled task %s\n", taskName)
	return nil
}

func (s taskService) logs() string { return `Get-Content -Wait "` + s.p.Log + `"` }

// asAdmin runs commands with admin rights: directly when elevated already,
// otherwise in one elevated PowerShell after a UAC prompt. Each command
// must succeed for the next to run.
func asAdmin(out io.Writer, why string, cmds ...[]string) error {
	if osutil.IsElevated() {
		for _, c := range cmds {
			if msg, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
				return fmt.Errorf("%s: %v: %s", strings.Join(c, " "), err, strings.TrimSpace(string(msg)))
			}
		}
		return nil
	}
	logf, err := os.CreateTemp("", "catflap-admin-*.log")
	if err != nil {
		return err
	}
	logPath := logf.Name()
	logf.Close()
	defer os.Remove(logPath)
	var script strings.Builder
	for _, c := range cmds {
		q := make([]string, len(c))
		for i, a := range c {
			q[i] = psQuote(a)
		}
		fmt.Fprintf(&script, "& %s *>> %s\nif ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }\n", strings.Join(q, " "), psQuote(logPath))
	}
	inner := base64.StdEncoding.EncodeToString(utf16LE(script.String()))
	outer := fmt.Sprintf(`try { $p = Start-Process -FilePath powershell.exe -ArgumentList '-NoProfile','-NonInteractive','-EncodedCommand','%s' -Verb RunAs -WindowStyle Hidden -Wait -PassThru -ErrorAction Stop; exit $p.ExitCode } catch { exit 1223 }`, inner)
	fmt.Fprintf(out, "    Windows asks for admin rights to %s: confirm the prompt (UAC) ...\n", why)
	err = exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", outer).Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ee) && ee.ExitCode() == 1223:
		return errors.New("no admin rights: the prompt was declined, or can't be shown in this session (over ssh: run this in an elevated PowerShell instead)")
	}
	msg, _ := os.ReadFile(logPath)
	return fmt.Errorf("could not %s: %v: %s", why, err, strings.TrimSpace(decodeOutput(msg)))
}

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func utf16LE(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, r := range u {
		binary.LittleEndian.PutUint16(b[2*i:], r)
	}
	return b
}

// utf16File is s as UTF-16LE with a byte order mark, as schtasks reads XML.
func utf16File(s string) []byte {
	return append([]byte{0xff, 0xfe}, utf16LE(s)...)
}

// decodeOutput turns PowerShell's redirected output (UTF-16LE in Windows
// PowerShell 5.1) into a string.
func decodeOutput(b []byte) string {
	if len(b) >= 2 && b[0] == 0xff && b[1] == 0xfe {
		b = b[2:]
		u := make([]uint16, len(b)/2)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(b[2*i:])
		}
		return string(utf16.Decode(u))
	}
	return string(b)
}
