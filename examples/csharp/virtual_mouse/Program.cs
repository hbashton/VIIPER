using Viiper.Client;
using Viiper.Client.Devices.Mouse;

if (args.Length < 1)
{
    Console.WriteLine("Usage: dotnet run -- <host> [port]");
    Console.WriteLine("Example: dotnet run -- localhost 3242");
    return;
}
var host = args[0];
var port = args.Length > 1 && int.TryParse(args[1], out var p) ? p : 3242;
var client = new ViiperClient(host, port);

// Find or create a bus
uint busId;
bool createdBus = false;
{
    var list = await client.BusListAsync();
    if (list.Buses.Length == 0)
    {
        Exception? lastErr = null;
        busId = 0;
        for (uint i = 1; i <= 100; i++)
        {
            try { var r = await client.BusCreateAsync(i.ToString()); busId = r.BusID; createdBus = true; break; }
            catch (Exception ex) { lastErr = ex; }
        }
        if (busId == 0) { Console.WriteLine($"BusCreate failed: {lastErr}"); return; }
        Console.WriteLine($"Created bus {busId}");
    }
    else { busId = list.Buses.Min(); Console.WriteLine($"Using existing bus {busId}"); }
}

// Add device and connect
string fullDevId; string devId; ViiperDevice device;
try
{
    var add = await client.BusDeviceAddAsync(busId, "mouse");
    fullDevId = add.ID; // e.g., "1-1"
    var parts = fullDevId.Split('-');
    devId = parts.Length > 1 ? parts[1] : fullDevId;
    device = await client.ConnectDeviceAsync(busId, devId);
    Console.WriteLine($"Created and connected to device {fullDevId} on bus {busId}");
}
catch (Exception ex)
{
    if (createdBus) { try { await client.BusRemoveAsync(busId.ToString()); } catch { } }
    Console.WriteLine($"AddDevice/connect error: {ex}");
    return;
}

AppDomain.CurrentDomain.ProcessExit += async (_, __) => await Cleanup();
Console.CancelKeyPress += async (_, e) => { e.Cancel = true; await Cleanup(); Environment.Exit(0); };

async Task Cleanup()
{
    try { await client.BusDeviceRemoveAsync(busId, devId); Console.WriteLine($"Removed device {fullDevId}"); } catch { }
    if (createdBus) { try { await client.BusRemoveAsync(busId.ToString()); Console.WriteLine($"Removed bus {busId}"); } catch { } }
}

// Send movement/click/scroll every 3s
var timer = new PeriodicTimer(TimeSpan.FromSeconds(3));
sbyte dir = 1; const sbyte step = 50;
Console.WriteLine("Every 3s: move diagonally by 50px, click left, scroll +1. Ctrl+C to stop.");
while (await timer.WaitForNextTickAsync())
{
    var dx = (sbyte)(step * dir);
    var dy = (sbyte)(step * dir);
    dir = (sbyte)-dir;

    await device.SendAsync(new MouseInput { Dx = dx, Dy = dy, Buttons = 0, Wheel = 0, Pan = 0 });
    Console.WriteLine($"→ Moved mouse dx={dx} dy={dy}");

    await Task.Delay(30);
    await device.SendAsync(new MouseInput { Buttons = 0, Dx = 0, Dy = 0, Wheel = 0, Pan = 0 }); // zero state

    await Task.Delay(50);
    await device.SendAsync(new MouseInput { Buttons = (byte)Btn_.Left, Dx = 0, Dy = 0, Wheel = 0, Pan = 0 });
    await Task.Delay(60);
    await device.SendAsync(new MouseInput { Buttons = 0, Dx = 0, Dy = 0, Wheel = 0, Pan = 0 });
    Console.WriteLine("→ Clicked (left)");

    await Task.Delay(50);
    await device.SendAsync(new MouseInput { Buttons = 0, Dx = 0, Dy = 0, Wheel = 1, Pan = 0 });
    await Task.Delay(30);
    await device.SendAsync(new MouseInput { Buttons = 0, Dx = 0, Dy = 0, Wheel = 0, Pan = 0 });
    Console.WriteLine("→ Scrolled (wheel=+1)");
}
