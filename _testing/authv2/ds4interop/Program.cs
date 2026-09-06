using System.Diagnostics;
using System.Net;
using System.Net.Sockets;
using DS4Windows;

// This executable links the two production DS4Windows source files unchanged.
// All configuration and the synthetic password must live in a pinned portable
// directory. It never loads DS4Windows, calls a driver, or reads installed keys.
if (args.Length != 2) throw new ArgumentException("Expected portable peer directory and independently recorded SHA256.");
string root = Path.GetFullPath(args[0]);
PortableLabContext.Initialize(new[] { "--portable-lab", args[1] }, root);
using PortableLabContext lab = PortableLabContext.Current;
if (File.ReadAllText(lab.KeyPath).Trim() != "synthetic-auth-v2-interop-not-a-deployment-key")
    throw new InvalidDataException("Only the public synthetic test password is permitted.");

using Process peer = new()
{
    StartInfo = new ProcessStartInfo(lab.ViiperPath)
    {
        WorkingDirectory = root,
        UseShellExecute = false,
        CreateNoWindow = true,
        RedirectStandardOutput = true,
        RedirectStandardError = true,
    }
};
if (!peer.Start()) throw new IOException("Could not start isolated peer.");
try
{
    string portLine = await peer.StandardOutput.ReadLineAsync().WaitAsync(TimeSpan.FromSeconds(10));
    if (!int.TryParse(portLine, out int port) || port is < 1 or > 65535)
        throw new InvalidDataException("Peer did not report a valid loopback port.");
    using TcpClient client = new() { NoDelay = true, ReceiveTimeout = 30_000, SendTimeout = 30_000 };
    await client.ConnectAsync(IPAddress.Loopback, port).WaitAsync(TimeSpan.FromSeconds(10));
    using Stream encrypted = ViiperAuthentication.Authenticate(client.GetStream());
    Task sending = Task.Run(() =>
    {
        for (int i = 0; i < 4096; i++)
        {
            byte[] payload = Payload(i, 0x5a);
            encrypted.Write(payload, 0, payload.Length);
        }
    });
    for (int i = 0; i < 4096; i++)
    {
        byte[] expected = Payload(i, 0xa5);
        byte[] actual = new byte[expected.Length];
        // Deliberately split plaintext reads to exercise retained record tails.
        int offset = 0;
        while (offset < actual.Length)
        {
            int n = encrypted.Read(actual, offset, Math.Min(7, actual.Length - offset));
            if (n == 0) throw new EndOfStreamException("Early peer EOF.");
            offset += n;
        }
        if (!actual.AsSpan().SequenceEqual(expected))
            throw new InvalidDataException($"Server plaintext mismatch at {i}.");
    }
    await sending.WaitAsync(TimeSpan.FromSeconds(30));
    await peer.WaitForExitAsync().WaitAsync(TimeSpan.FromSeconds(10));
    string output = await peer.StandardOutput.ReadToEndAsync();
    string errors = await peer.StandardError.ReadToEndAsync();
    if (peer.ExitCode != 0) throw new IOException($"Peer failed: {errors}");
    Console.Write(output);
    Console.WriteLine("PASS: production DS4Windows↔Go handshake and 8192 simultaneous directional records; no hardware involved");
}
finally
{
    if (!peer.HasExited) peer.Kill(entireProcessTree: false);
}

static byte[] Payload(int index, byte direction)
{
    byte[] data = new byte[24 + index % 521];
    for (int i = 0; i < data.Length; i++) data[i] = (byte)((byte)(index + i) ^ direction);
    return data;
}
