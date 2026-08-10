[CmdletBinding()]
param(
    [string]$ProjectPath,
    [string]$InfPath,
    [switch]$RequireStampedInf
)

$ErrorActionPreference = 'Stop'
if ([string]::IsNullOrWhiteSpace($ProjectPath)) {
    $ProjectPath = Join-Path $PSScriptRoot '..\driver\ViiperUde.vcxproj'
}
if ([string]::IsNullOrWhiteSpace($InfPath)) {
    $InfPath = Join-Path $PSScriptRoot '..\package\ViiperUde.inf'
}
$projectPathResolved = (Resolve-Path -LiteralPath $ProjectPath).Path
$infPathResolved = (Resolve-Path -LiteralPath $InfPath).Path
$driverSourceDirectory = Split-Path -Parent $projectPathResolved

[xml]$project = Get-Content -LiteralPath $projectPathResolved -Raw
$namespace = New-Object System.Xml.XmlNamespaceManager($project.NameTable)
$namespace.AddNamespace('msb', 'http://schemas.microsoft.com/developer/msbuild/2003')
$minorNodes = @($project.SelectNodes('//msb:KMDF_VERSION_MINOR', $namespace))
if ($minorNodes.Count -ne 2) {
    throw "Expected Debug and Release KMDF_VERSION_MINOR nodes; found $($minorNodes.Count)."
}
$minorVersions = @($minorNodes | ForEach-Object { $_.InnerText.Trim() } | Sort-Object -Unique)
if ($minorVersions.Count -ne 1 -or $minorVersions[0] -ne '27') {
    throw "Windows 10 1809 requires the committed KMDF 1.27 contract; project targets: $($minorVersions -join ', ')."
}

$majorNodes = @($project.SelectNodes('//msb:KMDF_VERSION_MAJOR', $namespace))
if ($majorNodes.Count -ne 2) {
    throw "Expected Debug and Release KMDF_VERSION_MAJOR nodes; found $($majorNodes.Count)."
}
$majorVersions = @($majorNodes | ForEach-Object { $_.InnerText.Trim() } | Sort-Object -Unique)
if ($majorVersions.Count -ne 1 -or $majorVersions[0] -ne '1') {
    throw "The committed driver must target KMDF major version 1; project targets: $($majorVersions -join ', ')."
}

function Get-SingleProjectValue([string]$elementName) {
    $nodes = @($project.SelectNodes("//msb:$elementName", $namespace))
    if ($nodes.Count -ne 1 -or [string]::IsNullOrWhiteSpace($nodes[0].InnerText)) {
        throw "Expected exactly one non-empty $elementName project value; found $($nodes.Count)."
    }
    return $nodes[0].InnerText.Trim()
}

$driverDate = Get-SingleProjectValue 'ViiperUdeDriverDate'
$driverVersion = Get-SingleProjectValue 'ViiperUdeDriverVersion'
$parsedDriverDate = [DateTime]::MinValue
if (-not [DateTime]::TryParseExact($driverDate, 'MM/dd/yyyy',
        [Globalization.CultureInfo]::InvariantCulture,
        [Globalization.DateTimeStyles]::None, [ref]$parsedDriverDate)) {
    throw "ViiperUdeDriverDate must use deterministic MM/dd/yyyy format; found '$driverDate'."
}
if ($driverVersion -notmatch '^\d+\.\d+\.\d+\.\d+$') {
    throw "ViiperUdeDriverVersion must be a four-part numeric version; found '$driverVersion'."
}

$infItems = @($project.SelectNodes('//msb:Inf', $namespace))
if ($infItems.Count -ne 1) {
    throw "Expected exactly one INF project item; found $($infItems.Count)."
}
$infItem = $infItems[0]
$stampContract = [ordered]@{
    'SpecifyDriverVerDirectiveDate' = 'true'
    'DateStamp' = '$(ViiperUdeDriverDate)'
    'SpecifyDriverVerDirectiveVersion' = 'true'
    'TimeStamp' = '$(ViiperUdeDriverVersion)'
}
foreach ($entry in $stampContract.GetEnumerator()) {
    $node = $infItem.SelectSingleNode("msb:$($entry.Key)", $namespace)
    if ($null -eq $node -or $node.InnerText.Trim() -cne $entry.Value) {
        throw "INF build metadata '$($entry.Key)' must be '$($entry.Value)' so StampInf cannot synthesize a date or version."
    }
}

$inf = Get-Content -LiteralPath $infPathResolved -Raw
if ($inf -notmatch '(?mi)^\[Standard\.NTamd64\.10\.0\.\.\.17763\]\s*$') {
    throw 'The INF no longer declares the reviewed Windows 10 1809 (build 17763) target floor.'
}
$driverVerPattern = '(?mi)^DriverVer\s*=\s*' +
    [regex]::Escape($driverDate) + '\s*,\s*' +
    [regex]::Escape($driverVersion) + '\s*$'
if ($inf -notmatch $driverVerPattern) {
    throw "The INF DriverVer must exactly match the deterministic project contract '$driverDate,$driverVersion'."
}
if ($inf -notmatch '(?mi)^\[ViiperUde_Install\.NT\.Wdf\]\s*$' -or
        $inf -notmatch '(?mi)^KmdfService\s*=\s*ViiperUde\s*,\s*ViiperUde_Wdf\s*$' -or
        $inf -notmatch '(?mi)^\[ViiperUde_Wdf\]\s*$') {
    throw 'The INF must bind the ViiperUde service through ViiperUde_Install.NT.Wdf.'
}
$expectedKmdfLibraryVersion = if ($RequireStampedInf) { '1.27' } else { '$KMDFVERSION$' }
$kmdfLibraryPattern = '(?mi)^KmdfLibraryVersion\s*=\s*' +
    [regex]::Escape($expectedKmdfLibraryVersion) + '\s*$'
if ($inf -notmatch $kmdfLibraryPattern) {
    throw "The INF KmdfLibraryVersion must be '$expectedKmdfLibraryVersion'."
}

# Keep the reviewed UdeCx callback and teardown contracts machine-verifiable.
# These checks intentionally target small invariants rather than formatting so
# a refactor cannot silently restore dispatch-level pageable callbacks or make
# parent cleanup call framework children that KMDF has already deleted.
$header = Get-Content -LiteralPath (Join-Path $driverSourceDirectory 'ViiperUde.h') -Raw
$controllerSource = Get-Content -LiteralPath (Join-Path $driverSourceDirectory 'Controller.c') -Raw
$deviceSource = Get-Content -LiteralPath (Join-Path $driverSourceDirectory 'Device.c') -Raw
$brokerSource = Get-Content -LiteralPath (Join-Path $driverSourceDirectory 'Broker.c') -Raw
$allDriverCSource = (Get-ChildItem -LiteralPath $driverSourceDirectory -Filter '*.c' |
    Sort-Object -Property FullName |
    ForEach-Object { Get-Content -LiteralPath $_.FullName -Raw }) -join "`n"

foreach ($requiredHeaderContract in @(
        'FAST_MUTEX DeviceLock;',
        'KEVENT BrokerOperationsDrained;',
        'KEVENT FileCleanupsDrained;',
        'volatile LONG ShuttingDown;')) {
    if (-not $header.Contains($requiredHeaderContract)) {
        throw "Missing native teardown contract in ViiperUde.h: $requiredHeaderContract"
    }
}
if ($controllerSource -notmatch
        'KeWaitForSingleObject\s*\(\s*&context->FileCleanupsDrained') {
    throw 'Terminal rundown must join any file cleanup admitted before ShuttingDown.'
}
if ($controllerSource -notmatch
        'pnpCallbacks\.EvtDeviceSelfManagedIoInit\s*=\s*ViiperEvtDeviceSelfManagedIoInit\s*;' -or
        $controllerSource -notmatch
        'pnpCallbacks\.EvtDeviceSelfManagedIoCleanup\s*=\s*ViiperEvtDeviceSelfManagedIoCleanup\s*;') {
    throw 'The controller must register both self-managed I/O initialization and terminal rundown callbacks.'
}
if ($controllerSource -notmatch
        'WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE\(&fileAttributes,\s*VIIPER_UDE_FILE_CONTEXT\);\s*fileAttributes\.ExecutionLevel\s*=\s*WdfExecutionLevelPassive\s*;') {
    throw 'File create/cleanup callbacks must explicitly run at PASSIVE_LEVEL.'
}
if ($deviceSource -notmatch
        'WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE\(&attributes,\s*VIIPER_UDE_DEVICE_CONTEXT\);[\s\S]{0,600}?attributes\.ExecutionLevel\s*=\s*WdfExecutionLevelPassive\s*;[\s\S]{0,300}?UdecxUsbDeviceCreate') {
    throw 'Every UdeCx USB-device object must explicitly request passive callback execution.'
}
if ($deviceSource -notmatch
        'WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE\(&attributes,\s*VIIPER_UDE_ENDPOINT_CONTEXT\);[\s\S]{0,600}?attributes\.ExecutionLevel\s*=\s*WdfExecutionLevelPassive\s*;[\s\S]{0,300}?UdecxUsbEndpointCreate') {
    throw 'Every UdeCx endpoint object must explicitly request passive callback execution.'
}
if ($deviceSource -notmatch
        'WDF_OBJECT_ATTRIBUTES_INIT_CONTEXT_TYPE\(&attributes,\s*UDECXUSBENDPOINT\);[\s\S]{0,300}?attributes\.ExecutionLevel\s*=\s*WdfExecutionLevelPassive\s*;[\s\S]{0,300}?WdfIoQueueCreate') {
    throw 'Every endpoint queue must explicitly run at PASSIVE_LEVEL for direct UdeCx completion.'
}
if (($controllerSource + $deviceSource + $brokerSource) -match
        'WdfWaitLock(?:Acquire|Release)\s*\([^;\r\n]*DeviceLock') {
    throw 'DeviceLock must remain embedded; a sibling WDF lock is unsafe during UdeCx child cleanup.'
}
if ($header -notmatch 'WDFWORKITEM\s+CompletionWorkItem\s*;' -or
        $brokerSource -notmatch
        'WDF_WORKITEM_CONFIG_INIT\s*\(\s*&workItemConfig\s*,\s*ViiperEvtCompletionWorkItem\s*\)') {
    throw 'UdeCx broker completion must use the preallocated passive completion work item.'
}
if ($allDriverCSource -match
        'CompletionDpc|ViiperEvtCompletionDpc|WdfDpc(?:Create|Enqueue|Cancel)|KeRaiseIrql') {
    throw 'UdeCx completion must never execute through a DPC or synthetic DISPATCH_LEVEL transition.'
}
$dpcCallbackNames = [regex]::Matches($header, 'EVT_WDF_DPC\s+(?<name>[A-Za-z_][A-Za-z0-9_]*)\s*;')
foreach ($dpcCallbackName in $dpcCallbackNames) {
    $callbackName = [regex]::Escape($dpcCallbackName.Groups['name'].Value)
    $dpcBody = [regex]::Match(
        $allDriverCSource,
        "(?ms)^VOID\s+$callbackName\s*\([^)]*\)\s*\{(?<body>.*?)^\}")
    if ($dpcBody.Success -and $dpcBody.Groups['body'].Value -match 'UdecxUrbComplete') {
        throw "WDF DPC callback '$($dpcCallbackName.Groups['name'].Value)' must not complete a UdeCx URB."
    }
}
$completionWorkerMatch = [regex]::Match(
    $brokerSource,
    '(?ms)^VOID\s+ViiperEvtCompletionWorkItem\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
if (-not $completionWorkerMatch.Success -or
        $completionWorkerMatch.Groups['body'].Value -notmatch 'UdecxUrbComplete' -or
        $completionWorkerMatch.Groups['body'].Value -notmatch
            'KeGetCurrentIrql\s*\(\s*\)\s*==\s*PASSIVE_LEVEL') {
    throw 'Could not verify that broker UdeCx completion is owned by the passive work item.'
}
$unownedCompletionMatch = [regex]::Match(
    $brokerSource,
    '(?ms)^VOID\s+ViiperCompleteUnownedUrb\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
if (-not $unownedCompletionMatch.Success -or
        $unownedCompletionMatch.Groups['body'].Value -notmatch 'UdecxUrbComplete' -or
        $unownedCompletionMatch.Groups['body'].Value -notmatch
            'KeGetCurrentIrql\s*\(\s*\)\s*==\s*PASSIVE_LEVEL') {
    throw 'Unowned endpoint URBs must complete directly under an asserted PASSIVE_LEVEL contract.'
}
$retrievedInputCompletionMatch = [regex]::Match(
    $deviceSource,
    '(?ms)^static\s+VOID\s+ViiperCompleteRetrievedInputUrb\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
if (-not $retrievedInputCompletionMatch.Success -or
        $retrievedInputCompletionMatch.Groups['body'].Value -match 'KeRaiseIrql' -or
        $retrievedInputCompletionMatch.Groups['body'].Value -notmatch
            'KeGetCurrentIrql\s*\(\s*\)\s*==\s*PASSIVE_LEVEL') {
    throw 'Retrieved input URBs must complete directly under an asserted PASSIVE_LEVEL contract.'
}
$allUdeCxCompletionCalls = [regex]::Matches(
    $allDriverCSource,
    'UdecxUrbComplete(?:WithNtStatus)?\s*\(').Count
$approvedUdeCxCompletionCalls =
    [regex]::Matches($completionWorkerMatch.Groups['body'].Value, 'UdecxUrbComplete(?:WithNtStatus)?\s*\(').Count +
    [regex]::Matches($unownedCompletionMatch.Groups['body'].Value, 'UdecxUrbComplete(?:WithNtStatus)?\s*\(').Count +
    [regex]::Matches($retrievedInputCompletionMatch.Groups['body'].Value, 'UdecxUrbComplete(?:WithNtStatus)?\s*\(').Count
if ($allUdeCxCompletionCalls -ne $approvedUdeCxCompletionCalls) {
    throw 'Every UdeCx completion call must remain inside an approved PASSIVE_LEVEL completion surface.'
}
$controllerCleanupMatch = [regex]::Match(
    $controllerSource,
    '(?ms)^VOID\s+ViiperEvtControllerCleanup\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
if (-not $controllerCleanupMatch.Success) {
    throw 'Could not locate ViiperEvtControllerCleanup for teardown validation.'
}
$forbiddenCleanupCalls = @(
    'WdfTimerStop',
    'WdfIoQueuePurgeSynchronously',
    'WdfSpinLockAcquire',
    'WdfWaitLockAcquire',
    'WdfWorkItemFlush',
    'ViiperPurgeOwnerOperations',
    'ViiperBeginControllerShutdown')
foreach ($forbiddenCall in $forbiddenCleanupCalls) {
    if ($controllerCleanupMatch.Groups['body'].Value.Contains($forbiddenCall)) {
        throw "Controller EvtCleanup must not call child-backed teardown routine '$forbiddenCall'."
    }
}
if ($controllerCleanupMatch.Groups['body'].Value -match '\b(?:Wdf|Udecx)[A-Za-z0-9_]*\s*\(') {
    throw 'Controller EvtCleanup must not call any WDF/UdeCx child-backed API.'
}
$selfManagedCleanupMatch = [regex]::Match(
    $controllerSource,
    '(?ms)^VOID\s+ViiperEvtDeviceSelfManagedIoCleanup\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
if (-not $selfManagedCleanupMatch.Success -or
        $selfManagedCleanupMatch.Groups['body'].Value -notmatch
            'ViiperPurgeOwnerOperations[\s\S]*BrokerOperationsDrained[\s\S]*WdfWorkItemFlush[\s\S]*ViiperBeginControllerShutdown') {
    throw 'Terminal rundown must drain and flush broker completion before asynchronous child teardown.'
}
$controllerShutdownMatch = [regex]::Match(
    $deviceSource,
    '(?ms)^VOID\s+ViiperBeginControllerShutdown\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
if (-not $controllerShutdownMatch.Success -or
        $controllerShutdownMatch.Groups['body'].Value -match 'KeWaitForSingleObject') {
    throw 'UdeCx child teardown must remain asynchronous and must not synchronously await child cleanup.'
}

$stampState = if ($RequireStampedInf) { 'stamped output' } else { 'source template' }
Write-Host "VIIPER UDE target and teardown contracts are aligned: Windows 10 1809, KMDF 1.27, deterministic DriverVer ($stampState)."
