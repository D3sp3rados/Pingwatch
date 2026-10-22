//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// psInstallUpdates: suchen, herunterladen, installieren (Windows-Update-Schnittstelle, wie
// „Alle herunterladen und installieren“). Funktionsupdates (Upgrades) und Treiber nie.
// security = Sicherheits-, kritische und Signatur-Updates sowie Update-Rollups (z. B. Tool zum Entfernen bösartiger Software).
const psInstallUpdates = psSearchFn + `$ErrorActionPreference='Stop'
try{
$s=New-Object -ComObject Microsoft.Update.Session; $s.ClientApplicationID='PingWacht'
$sec=@('0fa1201d-4330-4fa8-8ae9-b877473b6441','e6cf1350-c01b-414d-a61f-263d14d133b4','e0789628-ce08-4437-be74-2495b842f43b','28bc880e-0592-4cbf-8f95-c79b17911d5f')
$ok=@(); $fail=@(); $rb=$false; $seen=@{}; $any=$false
# je Update-Dienst getrennt suchen, laden und installieren (gemischte Quellen in einem Auftrag mag Windows nicht)
foreach($sid in (PWServices)){
  try{ $us=PWSearchIn $s $sid "IsInstalled=0 and IsHidden=0 and Type='Software'"; $any=$true }catch{ continue }
  $c=New-Object -ComObject Microsoft.Update.UpdateColl
  foreach($u in $us){
    $id=[string]$u.Identity.UpdateID; if($seen.ContainsKey($id)){ continue }; $seen[$id]=1
    $ids=@($u.Categories | ForEach-Object { ([string]$_.CategoryID).ToLower() })
    if($ids -contains '3689bdc8-b205-4af4-8d4a-a63924c5e9d5'){ continue }
    if('@@CAT@@' -ne 'all' -and -not ($ids | Where-Object { $sec -contains $_ })){ continue }
    if(-not $u.EulaAccepted){ $u.AcceptEula() }
    [void]$c.Add($u)
  }
  if($c.Count -eq 0){ continue }
  try{
    $d=$s.CreateUpdateDownloader(); $d.Updates=$c; [void]$d.Download()
    $i=$s.CreateUpdateInstaller(); $i.Updates=$c; $ir=$i.Install()
    for($k=0;$k -lt $c.Count;$k++){ $rc=$ir.GetUpdateResult($k).ResultCode; if($rc -eq 2 -or $rc -eq 3){ $ok+=[string]$c.Item($k).Title } else { $fail+=[string]$c.Item($k).Title } }
    if($ir.RebootRequired){ $rb=$true }
  }catch{ for($k=0;$k -lt $c.Count;$k++){ $fail+=([string]$c.Item($k).Title+' ('+$_.Exception.Message+')') } }
}
if(-not $any){ throw 'Windows Update nicht erreichbar' }
try{ if((New-Object -ComObject Microsoft.Update.SystemInfo).RebootRequired){ $rb=$true } }catch{}
ConvertTo-Json -Compress @{ok=$ok;fail=$fail;reboot=$rb}
}catch{ ConvertTo-Json -Compress @{error=[string]$_.Exception.Message} }`

func installUpdates(cat, reboot string) (any, error) {
	if cat != "all" {
		cat = "security"
	}
	b, err := runPS(3*time.Hour, strings.ReplaceAll(psInstallUpdates, "@@CAT@@", cat))
	out := string(b)
	i := strings.Index(out, "{")
	if i < 0 {
		if err != nil {
			return nil, errors.New("Windows Update: " + shortErr(err))
		}
		return nil, errors.New("Windows Update: keine Antwort")
	}
	var v struct {
		OK     json.RawMessage `json:"ok"`
		Fail   json.RawMessage `json:"fail"`
		Reboot bool            `json:"reboot"`
		Error  string          `json:"error"`
	}
	if json.Unmarshal([]byte(out[i:]), &v) != nil {
		return nil, errors.New("Windows Update: Antwort nicht lesbar")
	}
	if v.Error != "" {
		if strings.Contains(strings.ToLower(v.Error), "access") || strings.Contains(v.Error, "0x80070005") {
			return nil, errors.New("Windows Update: keine Administratorrechte – der Agent muss als Dienst bzw. als Administrator laufen")
		}
		return nil, errors.New("Windows Update: " + v.Error)
	}
	res := actionResult{OK: psList(v.OK), Fail: psList(v.Fail), Reboot: v.Reboot}
	if v.Reboot && (reboot == "ifneeded" || reboot == "now") || reboot == "now" {
		if rebootMachine(300) == nil {
			res.Planned = "in 5 Minuten"
		}
	}
	res.Message = actionSummary(len(res.OK), len(res.Fail), res.Reboot, res.Planned)
	return res, nil
}

// psList: PowerShell gibt einzelne Werte ohne Liste aus.
func psList(r json.RawMessage) []string {
	var l []string
	if json.Unmarshal(r, &l) == nil {
		return l
	}
	var one string
	if json.Unmarshal(r, &one) == nil && one != "" {
		return []string{one}
	}
	return nil
}

func rebootMachine(delaySec int) error {
	cmd := exec.Command("shutdown.exe", "/r", "/t", strconv.Itoa(delaySec), "/c", "PingWacht: Neustart nach Updates bzw. auf Anforderung", "/d", "p:2:17")
	hideWindow(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.New("Neustart fehlgeschlagen: " + strings.TrimSpace(string(out)))
	}
	return nil
}
