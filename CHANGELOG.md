# Changelog

Historical upstream release descriptions. Current downloads are on [Releases](https://github.com/hbashton/VIIPER/releases).

## Inherited release records

The following records preserve the original release descriptions and their
limitations. They are historical context, not current setup instructions.

<a id="inherited-release-notes-v016-rc464"></a>

### release-notes-v0.1.6-rc4.6.4

Original record: `docs/release-notes-v0.1.6-rc4.6.4.md`. Instructions and validation claims apply to that release.

### VIIPER 0.1.6-rc4.6.4 — Smoother Controller Feedback

This update accompanies DS4Windows RC4.6.4 and improves the consistency and detail of supported controller feedback.

#### Improvements

- Improved conversion of advanced game haptics for DualSense and DualSense Edge over Bluetooth when used with the matching DS4Windows update.
- More consistent controller-audio delivery through brief timing variations, helping prevent interrupted haptic effects.
- Backward-compatible operation with clients that do not request the updated conversion.

#### Install or update

**DS4Windows users:** install the matching **DS4Windows RC4.6.4 all-in-one installer**, or extract its complete portable ZIP. Both include the matching VIIPER build; a separate installation is not necessary.

Close your games, exit DS4Windows, and quit VIIPER before updating. Keep **USB/IP 0.9.7.7** when using DS4Windows.

Standalone users should select the archive for their operating system and architecture from the assets below. This is a release candidate.

Thanks for using VIIPER!

<a id="inherited-release-notes-v017-rc465"></a>

### release-notes-v0.1.7-rc4.6.5

Original record: `docs/release-notes-v0.1.7-rc4.6.5.md`. Instructions and validation claims apply to that release.

### VIIPER 0.1.7-rc4.6.5 — Updates Made Simple

VIIPER updates now come with DS4Windows. There are no separate VIIPER update pop-ups or automatic downloads.

Your existing settings remain compatible. Controller support and feedback are unchanged in this update.

#### How to update

**DS4Windows users:** use the matching DS4Windows RC4.6.5 installer or complete portable ZIP. Both include VIIPER, so you do not need a separate download.

Standalone users can download the archive for their operating system below. Close VIIPER before replacing its files.

This is a release candidate. Thanks for your feedback!

<a id="inherited-release-notes-v019-rc466"></a>

### release-notes-v0.1.9-rc4.6.6

Original record: `docs/release-notes-v0.1.9-rc4.6.6.md`. Instructions and validation claims apply to that release.

### VIIPER 0.1.9-rc4.6.6 — Reliable Startup & Better Edge Support

- Improved startup and shutdown cleanup so failed or canceled attempts can be retried more reliably.
- Added clearer startup failure information for DS4Windows.
- Improved virtual DualSense Edge reports and compatibility with the shared DualSense feedback path.
- Unsupported Edge onboard-profile commands now fail clearly instead of appearing to succeed.

**DS4Windows users:** use the matching RC4.6.6 installer or complete portable ZIP. Both include the correct VIIPER, so no separate update is needed.

Standalone users can download the archive for their operating system below. Close VIIPER before replacing its files.

This is a release candidate. Virtual Edge onboard-profile editing is not supported. Thanks for your feedback!
