// Offline/ephemeral-loopback tests of the generated TypeScript auth module.
// Usage: node typescript.cjs <absolute path to compiled auth.js>
const {test} = require('node:test');
const assert = require('node:assert/strict');
const {Duplex} = require('node:stream');
const {once} = require('node:events');
const {createServer} = require('node:net');
const {Socket} = require('node:net');
const crypto = require('node:crypto');
const {EncryptedSocket, performAuthHandshake, deriveKey, deriveSessionKey} = require(process.argv[2]);
const key = Buffer.from(Array.from({length:32}, (_, i) => i));
const payload = Buffer.from('000102037f80feff', 'hex');
const vectors = [
 '0000002400000000000000000000000018b94032d266582e05ebcfe4ba88b8a24dd1043e6dcd23fb',
 '00000024000000000000000000000001695d7eda4e8a46850d10d0c85e47680d9f125be025e25461',
 '00000024000000010000000000000000ab479fea760618c3be9f8fd13269fd4b4fc440460493639f',
 '00000024000000010000000000000001e10be70c805489fbcb0d12b623a633fd8e2ad6a2e57a58c0'
].map(s => Buffer.from(s, 'hex'));

class FakeSocket extends Duplex {
  written = [];
  _read() {}
  _write(chunk, encoding, callback) { this.written.push(Buffer.from(chunk)); callback(); }
  setNoDelay() { return this; }
}
function record(key, direction, counter, data) {
  const nonce = Buffer.alloc(12);
  nonce.writeUInt32BE(direction);
  nonce.writeBigUInt64BE(BigInt(counter), 4);
  const cipher = crypto.createCipheriv('chacha20-poly1305', key, nonce, {authTagLength:16});
  const body = Buffer.concat([nonce, cipher.update(data), cipher.final(), cipher.getAuthTag()]);
  const header = Buffer.alloc(4); header.writeUInt32BE(body.length);
  return Buffer.concat([header, body]);
}
function write(stream, data) { return new Promise((resolve,reject) => stream.write(data, e => e ? reject(e) : resolve())); }

test('session key fixes context and nonce order across languages', () => {
  assert.equal(deriveSessionKey(key,Buffer.alloc(32,0xa5),Buffer.alloc(32,0x5a)).toString('hex'),
    '71424901662650fb5c29ce71795ba055f114d4701da55490fadd27f82398f00c');
});

test('client writes match the cross-language vectors', {timeout:5000}, async () => {
  const socket = new FakeSocket(); const client = new EncryptedSocket(socket, key);
  await write(client, payload); await write(client, payload);
  assert.deepEqual(socket.written, vectors.slice(0,2));
  client.destroy();
});

test('fragmented server records preserve every byte across reads', {timeout:5000}, async () => {
  const socket = new FakeSocket(); const client = new EncryptedSocket(socket, key);
  const all = Buffer.concat(vectors.slice(2));
  for (const byte of all) socket.push(Buffer.from([byte]));
  socket.push(null);
  const actual = []; for await (const chunk of client) actual.push(chunk);
  assert.deepEqual(Buffer.concat(actual), Buffer.concat([payload,payload]));
});

for (const [name, frame] of [
  ['reflected', vectors[0]], ['counter gap', vectors[3]],
  ['short length', Buffer.from('0000001b','hex')],
  ['oversized length', Buffer.from('00200001','hex')],
  ['truncated body', vectors[2].subarray(0,39)],
  ['tampered tag', Buffer.concat([vectors[2].subarray(0,39),Buffer.from([vectors[2][39]^1])])]
]) test(`rejects ${name} and delivers no plaintext`, {timeout:5000}, async () => {
  const socket = new FakeSocket(); const client = new EncryptedSocket(socket, key);
  const received = []; client.on('data', b => received.push(b));
  const error = once(client, 'error');
  socket.push(frame); socket.push(null);
  await error;
  assert.equal(client.destroyed, true); assert.equal(received.length,0);
  assert.equal(client.recvCounter, BigInt(0));
});

test('rejects replay after one valid record', {timeout:5000}, async () => {
  const socket = new FakeSocket(); const client = new EncryptedSocket(socket, key);
  const received=[]; client.on('data', b => received.push(b));
  const error=once(client,'error');
  socket.push(Buffer.concat([vectors[2],vectors[2]]));
  await error; assert.deepEqual(Buffer.concat(received),payload);
});

test('retains complete records under backpressure through EOF', {timeout:5000}, async () => {
  const socket = new FakeSocket(); const client = new EncryptedSocket(socket,key);
  const first=Buffer.alloc(128*1024,0x51), second=Buffer.alloc(128*1024,0x52);
  socket.push(Buffer.concat([record(key,1,0,first),record(key,1,1,second)])); socket.push(null);
  // Let the first record fill the readable high-water mark before consuming it.
  await new Promise(resolve => setImmediate(resolve));
  const actual=[]; for await(const chunk of client) actual.push(chunk);
  assert.deepEqual(Buffer.concat(actual),Buffer.concat([first,second]));
});

test('empty records do not signal EOF', {timeout:5000}, async () => {
  const socket=new FakeSocket(); const client=new EncryptedSocket(socket,key);
  socket.push(Buffer.concat([record(key,1,0,Buffer.alloc(0)),record(key,1,1,payload)])); socket.push(null);
  const actual=[]; for await(const chunk of client) actual.push(chunk);
  assert.deepEqual(Buffer.concat(actual),payload);
});

test('counter remains exact above Number precision and refuses wrap', {timeout:5000}, async () => {
  const socket=new FakeSocket(); const client=new EncryptedSocket(socket,key);
  client.sendCounter=BigInt('9007199254740993');
  await write(client,payload);
  assert.equal(socket.written[0].readBigUInt64BE(8),BigInt('9007199254740993'));
  client.sendCounter=BigInt('18446744073709551615');
  client.on('error',()=>{});
  await assert.rejects(write(client,payload));
  assert.equal(socket.written.length,1); client.destroy();
});

async function withPeer(t, response) {
  const peers=[];
  const server=createServer(socket => {
    peers.push(socket); let request=Buffer.alloc(0);
    socket.on('data',chunk => {
      request=Buffer.concat([request,chunk]);
      if(request.length>=69) { socket.removeAllListeners('data'); response(socket,request); }
    });
  });
  server.listen(0,'127.0.0.1'); await once(server,'listening');
  const socket=new Socket();
  t.after(()=>{ socket.destroy(); for(const peer of peers) peer.destroy(); server.close(); });
  socket.connect(server.address().port,'127.0.0.1'); await once(socket,'connect');
  return socket;
}

test('handshake keeps a coalesced first encrypted record', {timeout:5000}, async t => {
  const password='auth-v2-synthetic-test-only';
  const socket=await withPeer(t,(peer,request)=>{
    assert.equal(request.subarray(0,5).toString(),'eVI2\0');
    const deploymentKey=deriveKey(password), clientNonce=request.subarray(5,37), serverNonce=Buffer.alloc(32,0x31);
    const tag=crypto.createHmac('sha256',deploymentKey).update('VIIPER-Auth-v2').update(clientNonce).digest();
    assert.deepEqual(request.subarray(37,69),tag);
    const session=deriveSessionKey(deploymentKey,serverNonce,clientNonce);
    peer.end(Buffer.concat([Buffer.from('OK\0'),serverNonce,record(session,1,0,payload)]));
  });
  const client=await performAuthHandshake(socket,password);
  const actual=[]; for await(const chunk of client) actual.push(chunk);
  assert.deepEqual(Buffer.concat(actual),payload);
});

for(const length of [1,3,5,34]) test(`rejects truncated ${length}-byte handshake`, {timeout:5000}, async t => {
  const socket=await withPeer(t,peer=>peer.end(Buffer.concat([Buffer.from('OK\0'),Buffer.alloc(32)]).subarray(0,length)));
  await assert.rejects(performAuthHandshake(socket,'synthetic-test-only'));
});
