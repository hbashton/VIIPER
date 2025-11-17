package csharp

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"text/template"
	"viiper/internal/codegen/meta"
)

const deviceTemplate = `{{writeFileHeader}}using System.IO;
using System.Net.Sockets;
using System.Threading.Channels;

namespace Viiper.Client;

/// <summary>
/// Interface for binary serializable input payloads sent to a VIIPER device stream.
/// </summary>
public interface IBinarySerializable
{
	void Write(BinaryWriter writer);
}

/// <summary>
/// Represents a live device stream connection allowing input sending and receiving raw output bytes.
/// </summary>
public sealed class ViiperDevice : IAsyncDisposable, IDisposable
{
	private readonly TcpClient _client;
	private readonly NetworkStream _stream;
	private readonly CancellationTokenSource _cts = new();
	private readonly Task _readLoop;
	private bool _disposed;

	/// <summary>
	/// Raised when output data is received from the device (raw binary frame).
	/// </summary>
	public event Action<byte[]>? OnOutput;

	internal ViiperDevice(TcpClient client, NetworkStream stream)
	{
		_client = client;
		_stream = stream;
		_readLoop = Task.Run(ReadLoopAsync);
	}

	/// <summary>
	/// Send a binary-serializable input payload to the device.
	/// </summary>
	public async Task SendAsync<T>(T payload, CancellationToken cancellationToken = default) where T : IBinarySerializable
	{
		ThrowIfDisposed();
		using var ms = new MemoryStream();
		using (var bw = new BinaryWriter(ms, System.Text.Encoding.UTF8, leaveOpen: true))
		{
			payload.Write(bw);
		}
		var buf = ms.ToArray();
		await _stream.WriteAsync(buf, 0, buf.Length, cancellationToken);
	}

	/// <summary>
	/// Send raw bytes to the device (advanced usage).
	/// </summary>
	public async Task SendRawAsync(byte[] data, CancellationToken cancellationToken = default)
	{
		ThrowIfDisposed();
		await _stream.WriteAsync(data, 0, data.Length, cancellationToken);
	}

	private async Task ReadLoopAsync()
	{
		var buffer = new byte[4096];
		try
		{
			while (!_cts.IsCancellationRequested)
			{
				var read = await _stream.ReadAsync(buffer, 0, buffer.Length, _cts.Token).ConfigureAwait(false);
				if (read <= 0)
				{
					break; // connection closed
				}
				var frame = new byte[read];
				Buffer.BlockCopy(buffer, 0, frame, 0, read);
				OnOutput?.Invoke(frame);
			}
		}
		catch (OperationCanceledException)
		{
			// normal during shutdown
		}
		catch (Exception)
		{
			// swallow; user can detect via absence of further events
		}
	}

	private void ThrowIfDisposed()
	{
		if (_disposed)
			throw new ObjectDisposedException(nameof(ViiperDevice));
	}

	/// <summary>
	/// Dispose synchronously.
	/// </summary>
	public void Dispose()
	{
		if (_disposed) return;
		_disposed = true;
		_cts.Cancel();
		try { _readLoop.Wait(); } catch { /* ignore */ }
		_stream.Dispose();
		_client.Dispose();
		_cts.Dispose();
		GC.SuppressFinalize(this);
	}

	/// <summary>
	/// Dispose asynchronously awaiting read loop completion.
	/// </summary>
	public async ValueTask DisposeAsync()
	{
		if (_disposed) return;
		_disposed = true;
		_cts.Cancel();
		try { await _readLoop.ConfigureAwait(false); } catch { /* ignore */ }
		_stream.Dispose();
		_client.Dispose();
		_cts.Dispose();
		GC.SuppressFinalize(this);
	}
}
`

func generateDevice(logger *slog.Logger, projectDir string, md *meta.Metadata) error {
	logger.Debug("Generating ViiperDevice stream wrapper")
	outputFile := filepath.Join(projectDir, "ViiperDevice.cs")
	tmpl := template.Must(template.New("device").Funcs(template.FuncMap{
		"writeFileHeader": writeFileHeader,
	}).Parse(deviceTemplate))
	f, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("create ViiperDevice.cs: %w", err)
	}
	defer f.Close()
	if err := tmpl.Execute(f, md); err != nil {
		return fmt.Errorf("execute device template: %w", err)
	}
	logger.Info("Generated ViiperDevice", "file", outputFile)
	return nil
}
