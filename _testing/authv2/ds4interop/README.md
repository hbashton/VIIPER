# DS4Windows ↔ Go authenticated transport interoperability

This opt-in harness links the production DS4Windows authentication stream and
portable-path policy unchanged. The separate Go peer imports VIIPER's production
auth package. It proves an actual cross-process handshake and 4,096 variable-sized
records in **each direction concurrently**, including fragmented plaintext reads.
It does not create an API server, virtual controller, USB/IP listener, or hardware
handle and is not a latency benchmark or whole-application acceptance test.

Use a new dedicated directory beneath the Desktop controller lab. Put the public
`synthetic.key.txt` fixture at `lab-data/viiper.key.txt` there. The harness rejects
any other password before launching the peer. Do not copy a real deployment key.

From VIIPER, using the project's Go compiler:

```powershell
go build -o '<new-peer-directory>/viiper.exe' ./_testing/authv2/interoppeer
dotnet publish ./_testing/authv2/ds4interop/Ds4Interop.csproj -c Release `
  -p:Ds4SourceRoot='<absolute-DS4Windows-repository>' -o '<new-harness-directory>'
Get-FileHash -LiteralPath '<new-peer-directory>/viiper.exe' -Algorithm SHA256
# Supply the independently recorded hash from the preceding build receipt:
& '<new-harness-directory>/Ds4Interop.exe' '<new-peer-directory>' '<SHA256>'
```

The peer is named `viiper.exe` only to exercise the existing pinned portable-path
policy. It is a small single-connection test program, **not** a production broker
and must never be staged into an application candidate. It listens only on an
OS-assigned ephemeral IPv4 loopback port, exits after one exchange, and imposes
30-second accept/I/O deadlines. The parent launches it hidden and cleans up only
that exact child process on failure. Passing output requires both processes to
verify every byte and the Go child to exit successfully.

Other tests in `internal/server/api/auth`, `DS4WindowsTests`, and this directory's
sibling harnesses cover version mismatch, wrong keys, invalid records, nonce
directions, replay, exhaustion and allocation. This happy-path integration check
does not replace those cases or tests of the complete API routing layer.
