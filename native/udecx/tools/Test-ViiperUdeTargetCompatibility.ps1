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
        'EX_PUSH_LOCK DeviceLock;',
        'ULONG InputDeviceCount;',
        'UDECXUSBDEVICE InputDevices[VIIPER_UDE_MAX_DEVICES];',
        'KEVENT BrokerOperationsDrained;',
        'KEVENT CompletionOperationsDrained;',
        'KEVENT FileCleanupsDrained;',
        'WDFDPC CompletionDpc;',
        'LIST_ENTRY CompletionQueue;',
        'volatile LONG PendingCompletions;',
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
    throw 'Every endpoint queue must explicitly run at PASSIVE_LEVEL for buffer preparation and broker admission.'
}
if (($controllerSource + $deviceSource + $brokerSource) -match
        'WdfWaitLock(?:Acquire|Release)\s*\([^;\r\n]*DeviceLock') {
    throw 'DeviceLock must remain embedded; a sibling WDF lock is unsafe during UdeCx child cleanup.'
}
if ($allDriverCSource -match 'Ex(?:Acquire|Release)FastMutex\s*\([^;\r\n]*DeviceLock') {
    throw 'DeviceLock must remain a shared/exclusive push lock; FAST_MUTEX serializes every input producer.'
}
foreach ($pushLockContract in @(
        'KeEnterCriticalRegion();',
        'ExAcquirePushLockShared(&ControllerContext->DeviceLock);',
        'ExAcquirePushLockExclusive(&ControllerContext->DeviceLock);',
        'ExReleasePushLockShared(&ControllerContext->DeviceLock);',
        'ExReleasePushLockExclusive(&ControllerContext->DeviceLock);',
        'KeLeaveCriticalRegion();')) {
    if (-not $header.Contains($pushLockContract)) {
        throw "DeviceLock lost required push-lock/APC contract: $pushLockContract"
    }
}
if ([regex]::Matches($header, '_IRQL_requires_max_\(APC_LEVEL\)').Count -lt 4) {
    throw 'Every shared/exclusive DeviceLock acquire/release helper must declare IRQL <= APC_LEVEL.'
}
if ($brokerSource -notmatch
        'WDF_DPC_CONFIG_INIT\s*\(\s*&dpcConfig\s*,\s*ViiperEvtCompletionDpc\s*\)[\s\S]{0,200}?dpcConfig\.AutomaticSerialization\s*=\s*WdfFalse\s*;[\s\S]{0,300}?WdfDpcCreate') {
    throw 'UdeCx completion must use one preallocated, nonserialized controller DPC.'
}
if ($controllerSource -notmatch
        'InitializeListHead\s*\(\s*&context->CompletionQueue\s*\)[\s\S]{0,300}?KeInitializeEvent\s*\(\s*&context->CompletionOperationsDrained') {
    throw 'The controller must initialize the intrusive completion queue and its drain event before broker creation.'
}
if ($allDriverCSource -match 'Ke(?:Raise|Lower)Irql') {
    throw 'The UdeCx completion boundary must be a real WDF DPC, never a synthetic IRQL transition.'
}
$completionDpcMatch = [regex]::Match(
    $brokerSource,
    '(?ms)^VOID\s+ViiperEvtCompletionDpc\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
if (-not $completionDpcMatch.Success -or
        $completionDpcMatch.Groups['body'].Value -notmatch
            'KeGetCurrentIrql\s*\(\s*\)\s*==\s*DISPATCH_LEVEL' -or
        $completionDpcMatch.Groups['body'].Value -match 'PAGED_CODE' -or
        $completionDpcMatch.Groups['body'].Value -notmatch
            'RemoveHeadList[\s\S]*UdecxUrbComplete[\s\S]*ViiperClearSlotLocked[\s\S]*ViiperEndpointOperationCompleted[\s\S]*InterlockedDecrement\s*\(\s*&controllerContext->PendingCompletions[\s\S]*KeSetEvent\s*\(\s*&controllerContext->CompletionOperationsDrained') {
    throw 'The completion DPC must run at exact DISPATCH_LEVEL, complete once, release ownership afterward, and signal final drain.'
}
$completionQueueMatch = [regex]::Match(
    $brokerSource,
    '(?ms)^BOOLEAN\s+ViiperQueueUrbCompletion\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
if (-not $completionQueueMatch.Success -or
        $completionQueueMatch.Groups['body'].Value -notmatch 'requestContext->CompletionQueued' -or
        $completionQueueMatch.Groups['body'].Value -notmatch
            'WdfObjectReference\s*\(\s*Request\s*\)' -or
        $completionQueueMatch.Groups['body'].Value -match
            'WdfObjectReference\s*\(\s*Endpoint\s*\)' -or
        $completionQueueMatch.Groups['body'].Value -notmatch
            'KeClearEvent\s*\(\s*&controllerContext->CompletionOperationsDrained\s*\)[\s\S]*InterlockedIncrement\s*\(\s*&controllerContext->PendingCompletions\s*\)[\s\S]*InsertTailList\s*\(\s*&controllerContext->CompletionQueue[\s\S]*WdfDpcEnqueue') {
    throw 'Completion admission must retain only the request, rely on pre-cleanup endpoint rundown, account drain, and enqueue the DPC.'
}
if ($completionDpcMatch.Groups['body'].Value -match
        'WdfObjectDereference\s*\(\s*endpoint\s*\)') {
    throw 'Completion DPC must not treat an endpoint WDF reference as permission to access an object after EvtCleanup.'
}
$unownedCompletionMatch = [regex]::Match(
    $brokerSource,
    '(?ms)^VOID\s+ViiperCompleteUnownedUrb\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
if (-not $unownedCompletionMatch.Success -or
        $unownedCompletionMatch.Groups['body'].Value -notmatch 'ViiperQueueUrbCompletion' -or
        $unownedCompletionMatch.Groups['body'].Value -match 'UdecxUrbComplete') {
    throw 'Rejected endpoint URBs must transfer terminal ownership to the shared completion DPC.'
}
$retrievedInputCompletionMatch = [regex]::Match(
    $deviceSource,
    '(?ms)^static\s+VOID\s+ViiperCompleteRetrievedInputUrb\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
if (-not $retrievedInputCompletionMatch.Success -or
        $retrievedInputCompletionMatch.Groups['body'].Value -notmatch 'ViiperQueueUrbCompletion' -or
        $retrievedInputCompletionMatch.Groups['body'].Value -match 'UdecxUrbComplete') {
    throw 'Retrieved fast-input URBs must transfer terminal ownership to the shared completion DPC.'
}
$allUdeCxCompletionCalls = [regex]::Matches(
    $allDriverCSource,
    'UdecxUrbComplete(?:WithNtStatus)?\s*\(').Count
$dpcUdeCxCompletionCalls = [regex]::Matches(
    $completionDpcMatch.Groups['body'].Value,
    'UdecxUrbComplete(?:WithNtStatus)?\s*\(').Count
if ($dpcUdeCxCompletionCalls -ne 2 -or
        $allUdeCxCompletionCalls -ne $dpcUdeCxCompletionCalls) {
    throw 'Every UdeCx URB terminal call must remain exclusively inside the DISPATCH_LEVEL completion DPC.'
}
$cancelMatch = [regex]::Match(
    $brokerSource,
    '(?ms)^VOID\s+ViiperEvtUrbCancel\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
if (-not $cancelMatch.Success -or
        $cancelMatch.Groups['body'].Value -notmatch
            'pending->State\s*=\s*ViiperUdePendingDpcCompletion[\s\S]*ViiperQueueUrbCompletion') {
    throw 'WDF cancellation must claim the slot exactly once before queueing its DISPATCH_LEVEL completion.'
}
$canceledOnQueueMatch = [regex]::Match(
    $brokerSource,
    '(?ms)^VOID\s+ViiperEvtUrbCanceledOnQueue\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
if ($deviceSource -notmatch
        'queueConfig\.EvtIoCanceledOnQueue\s*=\s*ViiperEvtUrbCanceledOnQueue\s*;' -or
        -not $canceledOnQueueMatch.Success -or
        $canceledOnQueueMatch.Groups['body'].Value -notmatch
            'ViiperEndpointOperationStarted\s*\(\s*endpoint\s*\)[\s\S]*ViiperQueueUrbCompletion') {
    throw 'Every endpoint queue must override synchronous queued cancellation and transfer it through endpoint rundown to the DPC.'
}
$endpointOperationStartMatch = [regex]::Match(
    $brokerSource,
    '(?ms)^VOID\s+ViiperEndpointOperationStarted\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
$endpointOperationCompleteMatch = [regex]::Match(
    $brokerSource,
    '(?ms)^VOID\s+ViiperEndpointOperationCompletedLocked\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
if (-not $endpointOperationStartMatch.Success -or
        $endpointOperationStartMatch.Groups['body'].Value -notmatch
            'if\s*\(\s*active\s*==\s*0\s*\)[\s\S]*KeClearEvent\s*\(\s*&endpointContext->OperationsDrained\s*\)[\s\S]*InterlockedIncrement\s*\(\s*&endpointContext->ActiveOperations\s*\)' -or
        -not $endpointOperationCompleteMatch.Success -or
        $endpointOperationCompleteMatch.Groups['body'].Value -notmatch
            'InterlockedDecrement\s*\(\s*&endpointContext->ActiveOperations\s*\)[\s\S]*if\s*\(\s*remaining\s*==\s*0\s*\)[\s\S]*KeSetEvent\s*\(\s*&endpointContext->OperationsDrained') {
    throw 'Endpoint ActiveOperations count/event transitions must remain linearized through the BrokerLock-owned helpers.'
}
$queueUrbMatch = [regex]::Match(
    $brokerSource,
    '(?ms)^NTSTATUS\s+ViiperQueueUrb\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
if (-not $queueUrbMatch.Success -or
        $queueUrbMatch.Groups['body'].Value -notmatch
            'ViiperEndpointOperationStarted\s*\(\s*endpoint\s*\)[\s\S]*ViiperAllocatePendingSlot' -or
        $queueUrbMatch.Groups['body'].Value -notmatch
            'queueCancelledCompletion[\s\S]*ViiperUdePendingDpcCompletion[\s\S]*ViiperQueueUrbCompletion') {
    throw 'URB admission must enter endpoint rundown before rejection and route mark-cancel races through the DPC.'
}
$purgeWorkItemMatch = [regex]::Match(
    $deviceSource,
    '(?ms)^VOID\s+ViiperEvtEndpointPurgeWorkItem\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
if (-not $purgeWorkItemMatch.Success -or
        $purgeWorkItemMatch.Groups['body'].Value -notmatch
            'KeWaitForSingleObject\s*\(\s*&endpointContext->OperationsDrained[\s\S]*UdecxUsbEndpointPurgeComplete') {
    throw 'Endpoint purge-complete must remain behind the forwarded-URB completion drain.'
}
if ($deviceSource -match 'WdfIoQueue(?:Purge|Start)\s*\(') {
    throw 'UdeCx owns the associated endpoint queue state; the client must only drain its forwarded paths.'
}
$completionDrainMatch = [regex]::Match(
    $brokerSource,
    '(?ms)^VOID\s+ViiperDrainUrbCompletions\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
if (-not $completionDrainMatch.Success -or
        $completionDrainMatch.Groups['body'].Value -notmatch
            'CompletionOperationsDrained[\s\S]*WdfDpcCancel\s*\(\s*controllerContext->CompletionDpc\s*,\s*TRUE\s*\)[\s\S]*IsListEmpty\s*\(\s*&controllerContext->CompletionQueue\s*\)[\s\S]*WdfDpcEnqueue') {
    throw 'Terminal DPC rundown must wait, join, verify the list, and re-arm work canceled before dispatch.'
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
            'ViiperPurgeOwnerOperations[\s\S]*BrokerOperationsDrained[\s\S]*ViiperDrainUrbCompletions[\s\S]*ViiperBeginControllerShutdown') {
    throw 'Terminal rundown must drain and join all completion-DPC ownership before asynchronous child teardown.'
}
$controllerShutdownMatch = [regex]::Match(
    $deviceSource,
    '(?ms)^VOID\s+ViiperBeginControllerShutdown\s*\([^)]*\)\s*\{(?<body>.*?)^\}')
if (-not $controllerShutdownMatch.Success -or
        $controllerShutdownMatch.Groups['body'].Value -match 'KeWaitForSingleObject') {
    throw 'UdeCx child teardown must remain asynchronous and must not synchronously await child cleanup.'
}

$stampState = if ($RequireStampedInf) { 'stamped output' } else { 'source template' }
Write-Host "VIIPER UDE target, DISPATCH completion, and teardown contracts are aligned: Windows 10 1809, KMDF 1.27, deterministic DriverVer ($stampState)."
