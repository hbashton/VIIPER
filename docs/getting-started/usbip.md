# USBIP

=== "Windows"

    VIIPER for DS4Windows requires the signed
    [usbip-win2 0.9.7.7 x64 release](https://github.com/vadimgrn/usbip-win2/releases/tag/v.0.9.7.7)
    with the matching DS4Windows-managed backend.
    Prefer the DS4Windows **Install / Repair VIIPER** action because it bundles
    and verifies this exact package. Older DS4Windows packages require their
    original dependency; update the complete DS4Windows package together.
    Do not substitute a
    different userspace/driver build; VIIPER rejects an incompatible ABI.

    ### Replacing a different driver version

    Close games and controller tools, then use the matching DS4Windows
    installer or **Install / Repair**. A driver replacement first removes the
    previous package, then requires a Windows restart before installing the
    new package. Run setup again after restarting. If the new driver requests
    another restart, complete it before using VIIPER.

    The 0.9.7.7 x64 installer and command-line executable have a valid Cloudyne
    Systems Authenticode signature. Both bundled driver binaries are signed by
    Microsoft Windows Hardware Compatibility Publisher. This upgrade does not
    require enabling test-signing mode or adding a test certificate.

    Windows attachments use upstream's `low-latency` WSK receive mode for
    frequent controller reports. Linux attachments keep the Linux usbip tools.
    The published 0.9.7.7 serial override has a known upstream bug; VIIPER leaves
    it empty and retains exact endpoint/export identity checks. See the
    [upstream release](https://github.com/vadimgrn/usbip-win2/releases/tag/v.0.9.7.7).

=== "Linux"

    ### Arch Linux

    ```bash
    sudo pacman -S usbip
    ```

    [Arch Wiki: USBIP](https://wiki.archlinux.org/title/USB/IP)

    ??? tip "Steam OS users"
        If you are installing VIIPER on Steam OS, switch to desktop mode and
        enable write access to the root filesystem first:

        ```bash
        sudo steamos-readonly disable
        ```

    ### Ubuntu/Debian

    ```bash
    sudo apt install linux-tools-generic
    ```

    [Ubuntu USBIP Manual](https://manpages.ubuntu.com/manpages/noble/man8/usbip.8.html)

    ### Linux kernel module setup

    !!! info "USBIP client requirement"
        USBIP requires the `vhci-hcd` (Virtual Host Controller Interface)
        kernel module on Linux. Most distributions include it but do not load
        it automatically.

    #### One-time setup

    ```bash
    echo "vhci-hcd" | sudo tee /etc/modules-load.d/vhci-hcd.conf
    sudo modprobe vhci-hcd
    ```

    #### Manual loading

    ```bash
    sudo modprobe vhci-hcd
    ```

    #### Verification

    ```bash
    lsmod | grep vhci_hcd
    ```
