[Console]::OutputEncoding=[Text.Encoding]::UTF8
$ProgressPreference='SilentlyContinue'
$ErrorActionPreference='SilentlyContinue'
$M='SSHMGR-INVENTORY'
function Sec($n,$b){"$M SECTION $n";try{&$b;"$M STATUS $n 0"}catch{"error`t$($_.Exception.Message)";"$M STATUS $n 1"}}
"$M BEGIN windows-v1"
Sec os {$k=Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion' -EA Stop
foreach($f in 'ProductName','DisplayVersion','ReleaseId','CurrentBuild','UBR','EditionID','InstallationType'){"$f`t$($k.$f)"}
"arch`t$env:PROCESSOR_ARCHITECTURE"}
Sec installed {foreach($v in @('64','HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*'),@('32','HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\*')){
Get-ItemProperty $v[1]|?{$_.DisplayName}|%{"$($v[0])`t$($_.DisplayName)`t$($_.DisplayVersion)`t$($_.Publisher)`t$($_.PSChildName)"}}}
$t=@(Get-NetTCPConnection -State Listen);$u=@(Get-NetUDPEndpoint)
Sec listen-tcp {if(-not $t){throw 'Get-NetTCPConnection failed'};$t|%{"$($_.LocalAddress)`t$($_.LocalPort)`t$($_.OwningProcess)"}}
Sec listen-udp {$u|%{"$($_.LocalAddress)`t$($_.LocalPort)`t$($_.OwningProcess)"}}
$ids=@(($t+$u)|%{$_.OwningProcess}|sort -Unique)
Sec processes {foreach($p in $ids){$r=Get-Process -Id $p;"$p`t$($r.ProcessName)`t$($r.Path)"}}
Sec services {Get-CimInstance Win32_Service -EA Stop|?{$ids -contains $_.ProcessId}|%{"$($_.ProcessId)`t$($_.Name)`t$($_.PathName)"}}
Sec firmware {$b=Get-CimInstance Win32_BIOS -EA Stop;"bios_vendor`t$($b.Manufacturer)";"bios_version`t$($b.SMBIOSBIOSVersion)";"bios_date`t$($b.ReleaseDate)"
$m=Get-CimInstance Win32_BaseBoard;"board_vendor`t$($m.Manufacturer)";"board_name`t$($m.Product)"
$c=Get-CimInstance Win32_ComputerSystem;"sys_vendor`t$($c.Manufacturer)";"product_name`t$($c.Model)"}
Sec gpu-driver {Get-CimInstance Win32_VideoController -EA Stop|%{"$($_.Name)`t$($_.DriverVersion)`t$($_.DriverDate)"}}
"$M END"
