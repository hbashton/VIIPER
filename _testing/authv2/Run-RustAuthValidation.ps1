param(
    [Parameter(Mandatory=$true)][string]$GeneratedAuthRoot,
    [Parameter(Mandatory=$true)][string]$PortableRustRoot,
    [Parameter(Mandatory=$true)][string]$AssemblerDirectory
)
$ErrorActionPreference = 'Stop'
$rustCompilerRoot = Join-Path $PortableRustRoot 'rustc-1.98.0-x86_64-pc-windows-gnu/rustc'
$rustCargo = Join-Path $PortableRustRoot 'cargo-1.98.0-x86_64-pc-windows-gnu/cargo/bin/cargo.exe'
$env:RUSTC = Join-Path $rustCompilerRoot 'bin/rustc.exe'
$env:RUSTDOC = Join-Path $rustCompilerRoot 'bin/rustdoc.exe'
$env:RUSTFLAGS = '-C link-self-contained=yes -C dlltool=' + (Join-Path $AssemblerDirectory 'dlltool.exe')
$env:CARGO_HOME = Join-Path $PortableRustRoot 'cargo-home'
$env:CARGO_TARGET_DIR = Join-Path (Split-Path -Parent $GeneratedAuthRoot) 'rust-auth-target'
$env:GENERATED_AUTH_ROOT = $GeneratedAuthRoot.Replace('\','/')
$env:PATH = (Join-Path $rustCompilerRoot 'bin') + ';' +
    (Join-Path $rustCompilerRoot 'lib/rustlib/x86_64-pc-windows-gnu/bin/self-contained') + ';' + $AssemblerDirectory + ';' + $env:PATH
& $rustCargo test --manifest-path (Join-Path $PSScriptRoot 'rust/Cargo.toml') --features async --quiet
if ($LASTEXITCODE -ne 0) { throw 'Generated Rust auth validation failed' }
