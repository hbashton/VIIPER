package typescript

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

const authUtilsTemplate = `// Auto-generated VIIPER TypeScript Client Library
// DO NOT EDIT - This file is generated from the VIIPER server codebase

import { Socket } from 'net';
import { createCipheriv, createDecipheriv, pbkdf2Sync, randomBytes, createHash, createHmac } from 'crypto';
import { Duplex } from 'stream';

const HANDSHAKE_MAGIC = 'eVI2\x00';
const NONCE_SIZE = 32;
const AUTH_CONTEXT = 'VIIPER-Auth-v2';
const SESSION_CONTEXT = 'VIIPER-Session-v2';
const PBKDF2_ITERATIONS = 100000;
const PBKDF2_SALT = 'VIIPER-Key-v1';
const MAX_PACKET_SIZE = 2 * 1024 * 1024;
const MAX_COUNTER = BigInt('18446744073709551615');

/**
 * Derive a 32-byte key from password using PBKDF2-SHA256
 */
function deriveKey(password: string): Buffer {
	if (!password || password.length === 0) {
		throw new Error('Password cannot be empty');
	}
	return pbkdf2Sync(password, PBKDF2_SALT, PBKDF2_ITERATIONS, 32, 'sha256');
}

/**
 * Derive session key from key and nonces using SHA-256
 */
function deriveSessionKey(key: Buffer, serverNonce: Buffer, clientNonce: Buffer): Buffer {
	const hash = createHash('sha256');
	hash.update(key);
	hash.update(serverNonce);
	hash.update(clientNonce);
	hash.update(Buffer.from(SESSION_CONTEXT));
	return hash.digest();
}

/**
 * Perform authentication handshake with VIIPER server
 * Returns a wrapped socket with encryption if handshake succeeds
 */
export async function performAuthHandshake(socket: Socket, password: string): Promise<Socket | EncryptedSocket> {
	const key = deriveKey(password);
	const clientNonce = randomBytes(NONCE_SIZE);
	const hmac = createHmac('sha256', key);
	hmac.update(Buffer.from(AUTH_CONTEXT));
	hmac.update(clientNonce);
	const authTag = hmac.digest();
	
	const handshakeMsg = Buffer.concat([
		Buffer.from(HANDSHAKE_MAGIC),
		clientNonce,
		authTag
	]);
	socket.write(handshakeMsg);
	
	const response = await readExactly(socket, 3 + NONCE_SIZE);
	
	const prefix = response.slice(0, 3).toString();
	if (prefix !== 'OK\x00') {
		socket.destroy();
		throw new Error('Authentication rejected; the client and server must both support VIIPER auth v2');
	}
	
	const serverNonce = response.slice(3);
	
	const sessionKey = deriveSessionKey(key, serverNonce, clientNonce);
	
	return new EncryptedSocket(socket, sessionKey);
}

/**
 * Read exact number of bytes from socket
 */
function readExactly(socket: Socket, length: number): Promise<Buffer> {
	return new Promise((resolve, reject) => {
		// Read only the handshake bytes; a coalesced first record stays buffered.
		socket.pause();
		const cleanup = () => {
			socket.removeListener('readable', onReadable);
			socket.removeListener('error', onError);
			socket.removeListener('end', onEnd);
			socket.removeListener('close', onEnd);
		};
		const onReadable = () => {
			const data = socket.read(length) as Buffer | null;
			if (data !== null) {
				cleanup();
				if (data.length !== length) reject(new Error('Truncated handshake response'));
				else resolve(data);
			}
		};
		const onError = (err: Error) => { cleanup(); reject(err); };
		const onEnd = () => onError(new Error('Connection closed before receiving full response'));
		socket.on('readable', onReadable);
		socket.on('error', onError);
		socket.on('end', onEnd);
		socket.on('close', onEnd);
		onReadable();
	});
}

/**
 * Encrypted socket wrapper using ChaCha20-Poly1305
 */
class EncryptedSocket extends Duplex {
	private socket: Socket;
	private sessionKey: Buffer;
	private sendCounter: bigint = BigInt(0);
	private recvCounter: bigint = BigInt(0);
	private recvBuffer: Buffer = Buffer.alloc(0);
	private socketEnded = false;
	
	constructor(socket: Socket, sessionKey: Buffer) {
		super();
		this.socket = socket;
		if (sessionKey.length !== 32) throw new Error('Session key must be 32 bytes');
		this.sessionKey = Buffer.from(sessionKey);
		
		socket.on('data', (chunk: Buffer) => this.handleIncomingData(chunk));
		socket.on('error', (err: Error) => this.destroy(err));
		socket.on('end', () => {
			this.socketEnded = true;
			this.handleIncomingData(Buffer.alloc(0));
		});
		socket.on('close', () => {
			if (!this.socketEnded) this.destroy(new Error('Encrypted transport closed unexpectedly'));
		});
		socket.resume();
	}
	
	_write(chunk: Buffer, encoding: string, callback: (error?: Error | null) => void): void {
		try {
			if (chunk.length === 0) { callback(); return; }
			if (chunk.length > MAX_PACKET_SIZE - 28 || this.sendCounter === MAX_COUNTER)
				throw new Error('Invalid encrypted record length or exhausted counter');
			const nonce = Buffer.alloc(12);
			// Client direction domain is zero; server uses one.
			nonce.writeBigUInt64BE(this.sendCounter, 4);
			this.sendCounter++;
			
			const cipher = createCipheriv('chacha20-poly1305', this.sessionKey, nonce, {
				authTagLength: 16
			});
			const ciphertext = Buffer.concat([cipher.update(chunk), cipher.final()]);
			const authTag = cipher.getAuthTag();

			const packet = Buffer.concat([nonce, ciphertext, authTag]);
			const lengthBuf = Buffer.alloc(4);
			lengthBuf.writeUInt32BE(packet.length, 0);
			
			this.socket.write(Buffer.concat([lengthBuf, packet]), callback);
		} catch (err) {
			callback(err as Error);
		}
	}
	
	_read(size: number): void {
		if (this.handleIncomingData(Buffer.alloc(0))) this.socket.resume();
	}
	
	private handleIncomingData(chunk: Buffer): boolean {
		if (this.destroyed) return false;
		this.recvBuffer = Buffer.concat([this.recvBuffer, chunk]);
		
		while (this.recvBuffer.length >= 4) {
			const packetLength = this.recvBuffer.readUInt32BE(0);
			if (packetLength < 28 || packetLength > MAX_PACKET_SIZE) {
				this.destroy(new Error('Invalid encrypted record length'));
				return false;
			}
			
			if (this.recvBuffer.length < 4 + packetLength) {
				break;
			}
			
			const packet = this.recvBuffer.slice(4, 4 + packetLength);
			this.recvBuffer = this.recvBuffer.slice(4 + packetLength);
			
			try {
				const nonce = packet.slice(0, 12);
				if (this.recvCounter === MAX_COUNTER || nonce.readUInt32BE(0) !== 1 ||
					nonce.readBigUInt64BE(4) !== this.recvCounter)
					throw new Error('Invalid encrypted record sequence or direction');
				const ciphertext = packet.slice(12, packet.length - 16);
				const authTag = packet.slice(packet.length - 16);
				
				const decipher = createDecipheriv('chacha20-poly1305', this.sessionKey, nonce, {
					authTagLength: 16
				});
				decipher.setAuthTag(authTag);
				
				const plaintext = Buffer.concat([decipher.update(ciphertext), decipher.final()]);
				this.recvCounter++;
				if (plaintext.length > 0 && !this.push(plaintext)) {
					this.socket.pause();
					return false;
				}
			} catch (err) {
				this.destroy(new Error('Invalid encrypted record'));
				return false;
			}
		}
		if (this.socketEnded) {
			if (this.recvBuffer.length !== 0) this.destroy(new Error('Truncated encrypted record'));
			else this.push(null);
		}
		return true;
	}
	
	setNoDelay(noDelay: boolean): this {
		this.socket.setNoDelay(noDelay);
		return this;
	}
	
	_final(callback: (error?: Error | null) => void): void {
		this.socket.end(callback);
	}

	_destroy(error: Error | null, callback: (error?: Error | null) => void): void {
		this.socket.destroy();
		this.sessionKey.fill(0);
		this.recvBuffer = Buffer.alloc(0);
		callback(error);
	}
}

export { EncryptedSocket, deriveKey, deriveSessionKey };
`

func generateAuthUtils(logger *slog.Logger, utilsDir string) error {
	logger.Debug("Generating auth.ts")
	outputFile := filepath.Join(utilsDir, "auth.ts")

	if err := os.WriteFile(outputFile, []byte(authUtilsTemplate), 0644); err != nil {
		return fmt.Errorf("write auth.ts: %w", err)
	}

	logger.Info("Generated auth.ts", "file", outputFile)
	return nil
}
