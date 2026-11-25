//go:build windows

package guiapp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Launch at login has two possible registrations and uses exactly one.
//
// The per-user Run key needs no admin rights but cannot elevate, so a TUN user
// clicks through a UAC prompt at every logon and then waits while the app hands
// over to its elevated copy. A logon scheduled task with RunLevel=Highest has
// neither cost, but only the elevated copy can create one -- which is fine,
// because an elevated copy is exactly what a TUN user already has running.
//
// So: the elevated copy upgrades the registration to a task (and drops the Run
// key, since keeping both would start two instances at logon), while the
// unelevated copy keeps using the Run key as before. If the task cannot be
// created -- no admin rights, a policy forbidding task registration -- the Run
// key is left alone, which still works, just with the prompt.
//
// The task is managed through PowerShell's ScheduledTasks module rather than
// schtasks.exe: schtasks is on many endpoint-security program blacklists (it is
// a classic persistence tool), and a blocked spawn would fail at boot with
// nothing but a warning in the log.
const launchTaskName = "YellowSocks"

// launchTaskArgs starts the tray-only copy straight into the elevated path
// (there is no predecessor to hand over from).
const launchTaskArgs = "--hidden --elevated-direct"

// psString quotes a value for a single-quoted PowerShell string literal.
func psString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func launchTaskPaths() (exe, dir string, err error) {
	exe, err = os.Executable()
	if err != nil {
		return "", "", fmt.Errorf("resolve executable: %w", err)
	}
	return exe, filepath.Dir(exe), nil
}

// launchTaskRegistered reports whether the task exists.
func launchTaskRegistered() bool {
	ps := fmt.Sprintf("if (Get-ScheduledTask -TaskName %s -ErrorAction SilentlyContinue) { exit 0 } else { exit 1 }",
		psString(launchTaskName))
	_, err := hiddenCommand("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).CombinedOutput()
	return err == nil
}

// ensureLaunchTask registers the logon task, or repairs it when the portable
// folder moved since it was written. It is a no-op when the task already points
// at this exe, so a boot start pays one PowerShell launch, not a rewrite.
func ensureLaunchTask() error {
	exe, dir, err := launchTaskPaths()
	if err != nil {
		return err
	}
	ps := fmt.Sprintf(`
$name = %s
$exe  = %s
$cur  = Get-ScheduledTask -TaskName $name -ErrorAction SilentlyContinue
if ($cur -and $cur.Actions[0].Execute -eq $exe) { exit 0 }
$act  = New-ScheduledTaskAction -Execute $exe -Argument %s -WorkingDirectory %s
$prin = New-ScheduledTaskPrincipal -UserId "$env:USERDOMAIN\$env:USERNAME" -LogonType Interactive -RunLevel Highest
$trg  = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"
$set  = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero)
Register-ScheduledTask -TaskName $name -Action $act -Principal $prin -Trigger $trg -Settings $set -Force | Out-Null
`, psString(launchTaskName), psString(exe), psString(launchTaskArgs), psString(dir))
	if out, err := hiddenCommand("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).CombinedOutput(); err != nil {
		return fmt.Errorf("register logon task: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func deleteLaunchTask() error {
	if !launchTaskRegistered() {
		return nil
	}
	ps := fmt.Sprintf("Unregister-ScheduledTask -TaskName %s -Confirm:$false -ErrorAction Stop", psString(launchTaskName))
	if out, err := hiddenCommand("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).CombinedOutput(); err != nil {
		return fmt.Errorf("delete logon task: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
