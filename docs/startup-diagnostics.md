# Local startup diagnostics

VIIPER checks the required USB/IP installation and its live driver ABI before
opening the USB/IP and authenticated API listeners. These checks remain required;
a failed or timed-out driver check is not permission to expose virtual devices.

The Windows CLI version and driver checks each have a 10-second process timeout
and a 250 ms redirected-pipe drain limit. The DS4Windows launcher permits 25
seconds for startup admission and returns immediately when authenticated
readiness succeeds. This is not an added delay during normal startup or input.

When the server exits during startup, the launcher can read these numeric process
exit codes without collecting stderr, credentials, driver output, or user paths:

| Exit code | Failure phase |
| --- | --- |
| 70 | Required USB/IP executable unavailable or version query failed |
| 71 | Installed USB/IP CLI version is not the required version |
| 72 | USB/IP prerequisite process or redirected pipe drain timed out |
| 73 | USB/IP driver probe failed or reported an incompatible ABI |
| 74 | API key path or key setup failed |
| 75 | USB/IP listener failed |
| 76 | API listener startup failed |

Other CLI/configuration errors retain their existing exit behavior. Numeric
failure codes are diagnostic information only: they never authorize process
termination, driver changes, credential replacement, or a different backend.
An older VIIPER build may return a generic code instead.

Successful startup still requires verified process ownership and the normal
authenticated API response. A process merely staying alive is not readiness.
