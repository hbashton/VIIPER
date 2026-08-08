$ErrorActionPreference = "Stop"

$installerPath = Join-Path $PSScriptRoot "install.ps1"
$tokens = $null
$parseErrors = $null
$ast = [Management.Automation.Language.Parser]::ParseFile(
    $installerPath, [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count -ne 0) {
    $parseErrors | Format-List
    throw "scripts/install.ps1 has PowerShell parser errors."
}

$installerSource = Get-Content -LiteralPath $installerPath -Raw
if ($installerSource -notmatch 'VIIPER_DEVELOPER_STANDALONE' -or
        $installerSource -notmatch 'Global\\DS4Windows-VIIPER-Setup') {
    throw "Standalone Windows setup is not fail-closed behind the managed transaction contract."
}

$helper = $ast.Find({
    param($node)
    $node -is [Management.Automation.Language.FunctionDefinitionAst] -and
        $node.Name -eq "Get-OptionalRegistryValue"
}, $true)
if ($null -eq $helper) {
    throw "The optional registry helper is missing."
}

# Load only the side-effect-free helper from the installer. Dot-sourcing the
# complete installer would download packages and mutate the machine.
Invoke-Expression $helper.Extent.Text

$root = [Microsoft.Win32.Registry]::CurrentUser
$testSubKey = "Software\VIIPER-Installer-FirstRun-Test-" +
    [Guid]::NewGuid().ToString("N")
try {
    $missingKey = Get-OptionalRegistryValue $root $testSubKey "VIIPER"
    if ($null -ne $missingKey) {
        throw "A missing registry key did not return null."
    }

    $key = $root.CreateSubKey($testSubKey, $true)
    try {
        $missingValue = Get-OptionalRegistryValue $root $testSubKey "VIIPER"
        if ($null -ne $missingValue) {
            throw "A missing registry value did not return null."
        }

        $expected = '"C:\Program Files\DS4Windows\VIIPER\viiper.exe" server --quiet'
        $key.SetValue("VIIPER", $expected,
            [Microsoft.Win32.RegistryValueKind]::String)
        $observed = Get-OptionalRegistryValue $root $testSubKey "VIIPER"
        if (-not [string]::Equals([string]$observed, $expected,
                [StringComparison]::Ordinal)) {
            throw "An existing registry value was not returned exactly."
        }
    }
    finally {
        if ($null -ne $key) { $key.Dispose() }
    }
}
finally {
    $root.DeleteSubKeyTree($testSubKey, $false)
}

Write-Host "VIIPER installer first-run registry contract passed."
