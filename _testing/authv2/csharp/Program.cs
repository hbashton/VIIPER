using System.Reflection;
using Viiper.Client;

byte[] key = Enumerable.Range(0,32).Select(i=>(byte)i).ToArray();
byte[] payload=Convert.FromHexString("000102037f80feff");
string[] vectors = {
 "0000002400000000000000000000000018b94032d266582e05ebcfe4ba88b8a24dd1043e6dcd23fb",
 "00000024000000000000000000000001695d7eda4e8a46850d10d0c85e47680d9f125be025e25461",
 "00000024000000010000000000000000ab479fea760618c3be9f8fd13269fd4b4fc440460493639f",
 "00000024000000010000000000000001e10be70c805489fbcb0d12b623a633fd8e2ad6a2e57a58c0"
};
static void Check(bool condition,string message) { if(!condition) throw new Exception(message); }
using(var wire=new MemoryStream())
using(var client=new EncryptedStream(wire,(byte[])key.Clone())) {
    await client.WriteAsync(payload); await client.WriteAsync(payload);
    Check(wire.ToArray().SequenceEqual(Convert.FromHexString(vectors[0]+vectors[1])),"client vectors differ");
}
using(var wire=new MemoryStream(Convert.FromHexString(vectors[2]+vectors[3])))
using(var client=new EncryptedStream(wire,(byte[])key.Clone())) {
    byte[] actual=new byte[16];
    for(int i=0;i<16;i++) Check(await client.ReadAsync(actual,i,1)==1,"lost record tail");
    Check(actual.SequenceEqual(payload.Concat(payload)),"server vectors differ");
}
foreach(string frame in new[]{vectors[0], vectors[3], "0000001b", "00200001", vectors[2][..^2]}) {
    using var wire=new MemoryStream(Convert.FromHexString(frame));
    using var client=new EncryptedStream(wire,(byte[])key.Clone());
    byte[] actual=Enumerable.Repeat((byte)0x55,8).ToArray();
    bool rejected=false;
    try { await client.ReadAsync(actual); } catch(Exception ex) when(ex is IOException or InvalidDataException) { rejected=true; }
    Check(rejected,"accepted malformed record");
    Check(actual.All(b=>b==0x55),"delivered bytes from invalid record");
}
using(var wire=new MemoryStream())
using(var client=new EncryptedStream(wire,(byte[])key.Clone())) {
    typeof(EncryptedStream).GetField("_sendCounter",BindingFlags.Instance|BindingFlags.NonPublic)!.SetValue(client,ulong.MaxValue);
    bool rejected=false; try { await client.WriteAsync(payload); } catch(Exception ex) when(ex is IOException or InvalidDataException) { rejected=true; }
    Check(rejected,"counter wrapped");
}
// Force overlapping async calls, then check the exact serialized record counters.
using(var wire=new YieldingWriteStream())
using(var client=new EncryptedStream(wire,(byte[])key.Clone())) {
    await Task.WhenAll(Enumerable.Range(0,128).Select(_=>client.WriteAsync(payload,0,payload.Length,CancellationToken.None)));
    byte[] packets=wire.ToArray(); Check(packets.Length==128*40,"lost concurrent write");
    for(int i=0;i<128;i++) Check(System.Buffers.Binary.BinaryPrimitives.ReadUInt64BigEndian(packets.AsSpan(i*40+8,8))==(ulong)i,"interleaved nonce or record");
}
Console.WriteLine("PASS: generated C# auth vectors, partial reads, malformed records, overflow, 128 concurrent writes");

sealed class YieldingWriteStream:MemoryStream {
    public override async Task WriteAsync(byte[] buffer,int offset,int count,CancellationToken cancellationToken) {
        await Task.Yield(); await base.WriteAsync(buffer,offset,count,cancellationToken);
    }
}
