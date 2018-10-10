package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/signer/v4"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type WssSession struct {
	SessionId  string
	StreamUrl  string
	TokenValue string
}

func buildRequest(serviceName, region, body string) (*http.Request, io.ReadSeeker) {
	reader := strings.NewReader(body)
	return buildRequestWithBodyReader(serviceName, region, reader)
}

func buildRequestWithBodyReader(serviceName, region string, body io.Reader) (*http.Request, io.ReadSeeker) {
	var bodyLen int

	type lenner interface {
		Len() int
	}
	if lr, ok := body.(lenner); ok {
		bodyLen = lr.Len()
	}

	endpoint := "https://" + serviceName + "." + region + ".amazonaws.com/"
	req, _ := http.NewRequest("POST", endpoint, body)
	req.Header.Set("X-Amz-Target", "AmazonSSM.StartSession")
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")

	if bodyLen > 0 {
		req.Header.Set("Content-Length", strconv.Itoa(bodyLen))
	}

	var seeker io.ReadSeeker
	if sr, ok := body.(io.ReadSeeker); ok {
		seeker = sr
	} else {
		seeker = aws.ReadSeekCloser(body)
	}

	return req, seeker
}

func buildSigner() v4.Signer {
	return v4.Signer{
		Credentials: credentials.NewStaticCredentials("XXXXX", "XXXXX", ""),
	}
}

func getSession() WssSession {
	req, body := buildRequest("ssm", "us-west-2", "{\"Target\":\"i-0123456789abc\"}")

	signer := buildSigner()

	_, err := signer.Sign(req, body, "ssm", "us-west-2", time.Now())
	if err != nil {
		panic(err)
	}

	tr := &http.Transport{
		IdleConnTimeout: 30 * time.Second,
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}

	b, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		panic(err)
	}

	var session WssSession

	err = json.Unmarshal(b, &session)
	if err != nil {
		panic(err)
	}

	return session
}

func constructResponse() []byte {
	str := "l"

	bytes := make([]byte, 19)

	bytes[3] = 0x74
	bytes = append(bytes, []byte("input_stream_data")...)

	strLen := uint32(len(str))
	strLenBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(strLenBytes, strLen)
	bytes = append(bytes, strLenBytes...)
	fourBytes := make([]byte, 4)
	bytes = append(bytes, fourBytes...) // sometimes a LE 1?

	//8 random bytes
	eightRandomBytes := make([]byte, 8) // seems timestamp related? 00 C3 8D C3 98...
	bytes = append(bytes, eightRandomBytes...)

	eightBytes := make([]byte, 8)
	eightBytes[7] = 0x04 // sequence
	bytes = append(bytes, eightBytes...)
	eightBytes = make([]byte, 8)
	bytes = append(bytes, eightBytes...)

	// 21 confusing bytes
	bytes = append(bytes, 0xc2)
	twelveBytes := make([]byte, 12) // 12 bytes?
	bytes = append(bytes, twelveBytes...)

	reqIdBytes := make([]byte, 8)
	rand.Read(reqIdBytes) // generate random bytes
	bytes = append(bytes, reqIdBytes...)

	hasher := sha256.New()
	hasher.Write([]byte(str))
	sha256str := hex.EncodeToString(hasher.Sum(nil))

	bytes = append(bytes, []byte(sha256str)...)
	eightBytes = make([]byte, 8)
	eightBytes[3] = 0x01
	eightBytes[7] = 0x01
	bytes = append(bytes, eightBytes...)
	bytes = append(bytes, []byte(str)...)

	print(hex.Dump(bytes))

	return bytes
}

func main() {
	session := getSession()

	fmt.Println(session)

	constructResponse()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	u, _ := url.Parse(session.StreamUrl)

	c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		log.Fatal("dial:", err)
	}
	defer c.Close()

	done := make(chan struct{})

	go func() {
		defer close(done)
		for {
			_, message, err := c.ReadMessage()
			if err != nil {
				log.Println("read:", err)
				return
			}
			//log.Printf("recv: %s", hex.Dump(message))
			//log.Printf("recv: %s", message[78:94])
			log.Printf("recv: %s", message[112:])

		}
	}()

	ticker := time.NewTicker(time.Second * 10)
	defer ticker.Stop()

	initMessage, _ := json.Marshal(map[string]string{
		"MessageSchemaVersion": "1.0",
		"RequestId":            uuid.New().String(),
		"TokenValue":           session.TokenValue,
	})
	c.WriteMessage(websocket.TextMessage, []byte(initMessage))

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			//err := c.WriteMessage(websocket.TextMessage, []byte("__ping__"))
			err := c.WriteMessage(websocket.TextMessage, constructResponse())
			fmt.Println("PING!")
			if err != nil {
				log.Println("write:", err)
				return
			}
		case <-interrupt:
			log.Println("interrupt")

			// Cleanly close the connection by sending a close message and then
			// waiting (with timeout) for the server to close the connection.
			err := c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			if err != nil {
				log.Println("write close:", err)
				return
			}
			select {
			case <-done:
			case <-time.After(time.Second):
			}
			return
		}
	}
}
