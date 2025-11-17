package csharp

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"viiper/internal/codegen/meta"
	"viiper/internal/codegen/scanner"
)

const clientTemplate = `{{writeFileHeader}}using System.Net.Sockets;
using System.Text;
using System.Text.Json;
using Viiper.Client.Types;

namespace Viiper.Client;

/// <summary>
/// VIIPER management API client for bus and device control
/// </summary>
public class ViiperClient : IDisposable
{
    private readonly string _host;
    private readonly int _port;
    private bool _disposed;

    /// <summary>
    /// Creates a new VIIPER client instance
    /// </summary>
    /// <param name="host">VIIPER server hostname or IP address</param>
    /// <param name="port">VIIPER API server port (default: 3242)</param>
    public ViiperClient(string host, int port = 3242)
    {
        _host = host ?? throw new ArgumentNullException(nameof(host));
        _port = port;
    }
{{range .Routes}}{{if eq .Method "Register"}}
    /// <summary>
    /// {{.Handler}}: {{.Path}}
    /// </summary>{{range .Arguments}}
    /// <param name="{{toCamelCase .Name}}">{{.Type}}</param>{{end}}{{if .ResponseDTO}}
    /// <returns>{{.ResponseDTO}}</returns>{{end}}
    public async Task<{{if .ResponseDTO}}{{.ResponseDTO}}{{else}}bool{{end}}> {{.Handler}}Async({{generateMethodParams .}}CancellationToken cancellationToken = default)
    {
        var path = "{{.Path}}"{{range $key, $value := .PathParams}}.Replace("{{lb}}{{$key}}{{rb}}", {{toCamelCase $key}}.ToString()){{end}};
        {{if .Arguments}}string? payload = {{range $i, $arg := .Arguments}}{{if $i}} + " " + {{end}}{{toCamelCase $arg.Name}}{{if ne $arg.Type "string"}}?.ToString(){{end}}{{end}};{{else}}string? payload = null;{{end}}
        {{if .ResponseDTO}}return await SendRequestAsync<{{.ResponseDTO}}>(path, payload, cancellationToken);{{else}}await SendRequestAsync<object>(path, payload, cancellationToken);
        return true;{{end}}
    }
{{end}}{{end}}
    private async Task<T> SendRequestAsync<T>(string path, string? payload, CancellationToken cancellationToken)
    {
        using var client = new TcpClient();
        await client.ConnectAsync(_host, _port, cancellationToken);
        
        using var stream = client.GetStream();
        
        // Build command line: "path arg1 arg2 ...\n" (matches Go transport protocol)
        string commandLine = path.ToLowerInvariant();
        if (!string.IsNullOrEmpty(payload))
        {
            commandLine += " " + payload;
        }
        commandLine += "\n";
        
        var requestBytes = Encoding.UTF8.GetBytes(commandLine);
        await stream.WriteAsync(requestBytes, cancellationToken);
        
        var buffer = new byte[8192];
        var responseBuilder = new StringBuilder();
        int bytesRead;
        
        while ((bytesRead = await stream.ReadAsync(buffer, cancellationToken)) > 0)
        {
            responseBuilder.Append(Encoding.UTF8.GetString(buffer, 0, bytesRead));
            if (responseBuilder.ToString().Contains('\n'))
                break;
        }
        
        var responseJson = responseBuilder.ToString().TrimEnd('\n');
        var response = JsonSerializer.Deserialize<T>(responseJson) 
            ?? throw new InvalidOperationException("Failed to deserialize response");
        
        return response;
    }

    /// <summary>
    /// Creates a device stream connection for sending input and receiving output
    /// </summary>
    /// <param name="busId">Bus ID</param>
    /// <param name="devId">Device ID</param>
    /// <param name="cancellationToken">Cancellation token</param>
    /// <returns>ViiperDevice stream wrapper</returns>
    public async Task<ViiperDevice> ConnectDeviceAsync(uint busId, string devId, CancellationToken cancellationToken = default)
    {
        var client = new TcpClient();
        await client.ConnectAsync(_host, _port, cancellationToken);
        
        var stream = client.GetStream();
        var streamPath = $"bus/{{lb}}busId{{rb}}/{{lb}}devId{{rb}}\n";
        var handshake = Encoding.UTF8.GetBytes(streamPath);
        await stream.WriteAsync(handshake, cancellationToken);
        
        return new ViiperDevice(client, stream);
    }

    public void Dispose()
    {
        if (_disposed) return;
        _disposed = true;
        GC.SuppressFinalize(this);
    }
}
`

// generateClient creates the ViiperClient management API class
func generateClient(logger *slog.Logger, projectDir string, md *meta.Metadata) error {
	logger.Debug("Generating ViiperClient management API")

	outputFile := filepath.Join(projectDir, "ViiperClient.cs")

	funcMap := template.FuncMap{
		"toCamelCase":          toCamelCase,
		"writeFileHeader":      writeFileHeader,
		"generateMethodParams": generateMethodParams,
		"lb":                   func() string { return "{" },
		"rb":                   func() string { return "}" },
	}

	tmpl, err := template.New("client").Funcs(funcMap).Parse(clientTemplate)
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}

	f, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	data := struct {
		Routes []scanner.RouteInfo
	}{
		Routes: md.Routes,
	}

	if err := tmpl.Execute(f, data); err != nil {
		return fmt.Errorf("execute template: %w", err)
	}

	logger.Info("Generated ViiperClient", "file", outputFile)
	return nil
}

func generateMethodParams(route scanner.RouteInfo) string {
	var params []string

	for key := range route.PathParams {
		params = append(params, fmt.Sprintf("uint %s", toCamelCase(key)))
	}

	for _, arg := range route.Arguments {
		csharpType := goTypeToCSharp(arg.Type)
		if arg.Optional {
			csharpType += "?"
		}
		params = append(params, fmt.Sprintf("%s %s", csharpType, toCamelCase(arg.Name)))
	}

	if len(params) == 0 {
		return ""
	}

	return strings.Join(params, ", ") + ", "
}
