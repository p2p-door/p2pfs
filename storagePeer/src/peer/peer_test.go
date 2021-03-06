package peer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dgrijalva/jwt-go"
	"google.golang.org/grpc"
)

func genIP() func() string {
	port := 9000
	return func() string {
		ip := fmt.Sprintf("127.0.0.1:%d", port)
		port++
		return ip
	}
}

var IP = genIP()

////////
// Service funcs
////////

// Generate random string with specified length
func randString(length int) []byte {

	randString := make([]byte, length)
	for i := range randString {
		randString[i] = byte(rand.Intn(256))
	}

	return randString
}

// Make one peer
func makePeer() (string, uint64, PeerServiceClient, *grpc.ClientConn, error) {
	ownIP := IP()

	ringsz := uint64(1000)
	NewPeer(ownIP, ownIP, ringsz, "", time.Second)

	connection, err := grpc.Dial(ownIP, grpc.WithInsecure())
	if err != nil {
		return "", 0, nil, nil, err
	}

	client := NewPeerServiceClient(connection)

	return ownIP, ringsz, client, connection, nil
}

// Make n peers in one ring
func makeRing(n uint) (string, uint64) {

	ringsz := uint64(1000)
	host := IP()

	NewPeer(host, host, ringsz, "", time.Second)

	ips := make([]string, n)
	for i := uint(0); i < n; i++ {
		ips[i] = IP()
		NewPeer(ips[i], ips[i], ringsz, host, time.Second)
	}

	return host, ringsz
}

// Generate a certificate
func genCertificate(fname string, fsize int64, act int8) (string, error) {
	key := []byte("qwertyuiopasdfghjklzxcvbnm123456")
	token := jwt.NewWithClaims(jwt.SigningMethodHS512, &FileClaim{Name: fname, Size: fsize, Act: act})

	tokenString, err := token.SignedString(key)
	if err != nil {
		return "", err
	}

	return tokenString, nil
}

// TestRW tests read/write capabilities of a peer
func TestRW(t *testing.T) {

	_, _, client, connection, err := makePeer()
	defer connection.Close()
	if err != nil {
		t.Error(err)
	}

	fName := "test_file"

	wstream, err := client.Write(context.Background())
	if err != nil {
		t.Error("Creating write stream failed:", err)
	}

	chunkAmnt := rand.Intn(16) + 16
	fLength := chunkAmnt * 8
	fContent := make([]byte, 0)
	writeCert, err := genCertificate(fName, int64(fLength), WRITACT)
	if err != nil {
		t.Error("Error creating write certificate:", err)
	}

	if err := wstream.Send(&WriteRequest{Name: fName, Certificate: writeCert}); err != nil {
		t.Error("Initializing write stream failed:", err)
	}

	for i := 0; i < chunkAmnt; i++ {
		nextChunk := randString(8)
		fContent = append(fContent, nextChunk...)
		if err = wstream.Send(&WriteRequest{Data: nextChunk}); err != nil {
			t.Error("Error writing to stream:", err)

		}
	}

	lastChunkLen := rand.Intn(4) + 3
	lastChunk := randString(lastChunkLen)

	fLength += lastChunkLen
	fContent = append(fContent, lastChunk...)

	if err := wstream.Send(&WriteRequest{Data: lastChunk}); err != nil {
		t.Error("Error writing final bytes to stream:", err)
	}

	writeReply, err := wstream.CloseAndRecv()
	if err != nil {
		t.Error("Error closing write stream!", err)
	}

	written := int(writeReply.Written)

	readCert, err := genCertificate(fName, int64(fLength), READACT)
	if err != nil {
		t.Error("Error creating read certificate!", err)
	}
	rstream, err := client.Read(context.Background(), &ReadRequest{Name: fName, ChunkSize: 8, Certificate: readCert})
	if err != nil {
		t.Error("Creating read stream failed:", err)
	}

	readContent := make([]byte, 0)
	for {
		readReply, err := rstream.Recv()

		if err == io.EOF {
			break
		}

		if err != nil {
			t.Error("Error reading from stream:", err)
		}

		nextChunk := readReply.Data[:readReply.Size]

		readContent = append(readContent, nextChunk...)
	}

	if r, w := len(readContent), written; r != w {
		t.Error("Read", r, "Wrote", w, " - doesn't match!")
	}

	for i, b := range readContent {
		if b != fContent[i] {
			t.Error("Read data different from written data")
		}
	}

	os.Remove(fName)
}

func TestUpload(t *testing.T) {

	ownIP, ringsz, _, connection, err := makePeer()
	defer connection.Close()
	if err != nil {
		t.Error(err)
	}

	fcontent := randString(4096)
	fname := "testfile.txt"
	shardSize := int64(len(fcontent))
	wCert, err := genCertificate(fname, shardSize, WRITACT)
	if err != nil {
		t.Error("Error creating write certificate!", err)
	}

	err = uploadFile(ownIP, fname, ringsz, fcontent, wCert)
	if err != nil {
		t.Error("Unable to send file", err)
	}

	fcontentRead, err := ioutil.ReadFile(fname)
	if err != nil {
		t.Error("Unable to read sent file", err)
	}

	if len(fcontentRead) != len(fcontent) {
		t.Error("Lengths don't match: written", len(fcontent), "read", len(fcontentRead))
	}

	for i, b := range fcontentRead {
		if fcontent[i] != b {
			t.Error("Content doesn't match!")
		}
	}

	os.Remove(fname)
}

func TestDownload(t *testing.T) {

	ownIP, ringsz, _, connection, err := makePeer()
	defer connection.Close()
	if err != nil {
		t.Error(err)
	}

	fcontent := randString(4096)
	fname := "testfile.txt"
	shardSize := int64(len(fcontent))
	rCert, err := genCertificate(fname, shardSize, READACT)
	if err != nil {
		t.Error("Error creating read certificate!", err)
	}

	ioutil.WriteFile(fname, fcontent, 0644)

	fcontentRead := make([]byte, len(fcontent))
	empty, err := downloadFile(ownIP, fname, ringsz, fcontentRead, rCert)
	if err != nil {
		t.Error("Unable to download file", err)
	}

	if empty != 0 {
		t.Error("Lengths don't match: empty =", empty)
	}

	for i, b := range fcontentRead {
		if fcontent[i] != b {
			t.Error("Content doesn't match!")
		}
	}

	os.Remove(fname)
}

func TestUD(t *testing.T) {
	ownIP, ringsz, _, connection, err := makePeer()
	if err != nil {
		t.Error(err)
	}
	defer connection.Close()

	fcontent := randString(4096)
	fname := "testfile.txt"
	shardSize := int64(len(fcontent))

	wCert, err := genCertificate(fname, shardSize, WRITACT)
	if err != nil {
		t.Error("Error creating write certificate!", err)
	}
	err = uploadFile(ownIP, fname, ringsz, fcontent, wCert)
	if err != nil {
		t.Error("Unable to send file", err)
	}

	fcontentRead := make([]byte, len(fcontent))
	rCert, err := genCertificate(fname, shardSize, READACT)
	if err != nil {
		t.Error("Error creating read certificate!", err)
	}

	empty, err := downloadFile(ownIP, fname, ringsz, fcontentRead, rCert)
	if err != nil {
		t.Error("Unable to download file", err)
	}

	if empty != 0 {
		t.Error("Lengths don't match: empty =", empty)
	}

	for i, b := range fcontentRead {
		if fcontent[i] != b {
			t.Error("Content doesn't match!")
		}
	}

	dCert, err := genCertificate(fname, shardSize, DELEACT)
	if err != nil {
		t.Error("Error creating delete certificate!", err)
	}

	if err = deleteFile(ownIP, fname, ringsz, dCert); err != nil {
		t.Error("Error deleting file", err)
	}
}

func findBin() (string, error) {
	absPath, err := filepath.Abs("./")
	if err != nil {
		return "", fmt.Errorf("Abs error: %s", err.Error())
	}

	projName := "storagePeer"
	projPathInd := strings.LastIndex(absPath, projName)
	projPath := absPath[:projPathInd+len(projName)]

	return projPath + "/bin", nil
}

func TestC(t *testing.T) {
	ip, ringsz, _, conn, err := makePeer()
	if err != nil {
		t.Error("Unable to create peer", err)
	}
	defer conn.Close()

	binpath, err := findBin()
	if err != nil {
		t.Error("Unable to find file:", err)
	}

	var errStream bytes.Buffer
	cTest := exec.Cmd{Path: "./c_test", Dir: binpath, Args: []string{binpath + "/c_test", ip, strconv.Itoa(int(ringsz))}, Stderr: &errStream}
	err = cTest.Run()
	if err != nil {
		t.Error("Run error:", err, "stderr:", errStream.String())
	}
}

func TestRSC(t *testing.T) {
	host, ringsz := makeRing(10)

	fname := "testfile"
	fcontent := randString(4096)
	shardSize := int64(len(fcontent)) / dataRSC
	wCert, err := genCertificate(fname, shardSize, WRITACT)
	if err != nil {
		t.Error("Error creating write certificate!", err)
	}

	rCert, err := genCertificate(fname, shardSize, READACT)
	if err != nil {
		t.Error("Error creating read certificate!", err)
	}

	dCert, err := genCertificate(fname, shardSize, DELEACT)
	if err != nil {
		t.Error("Error creating delete certificate!", err)
	}

	err = UploadFileRSC(host, fname, ringsz, fcontent, wCert)
	if err != nil {
		t.Error("UploadRSC error:", err)
	}

	f1 := rand.Intn(10)
	f2 := (f1 + rand.Intn(9) + 1) % 10
	os.Remove(fmt.Sprintf("%s_rep%d", fname, f1))
	os.Remove(fmt.Sprintf("%s_rep%d", fname, f2))

	fcontentRead := make([]byte, len(fcontent)*2)

	empty, err := DownloadFileRSC(host, fname, ringsz, fcontentRead, rCert)
	if err != nil {
		t.Error("DownloadRSC error:", err)
	}

	if empty < 0 {
		t.Error("File read too large, empty =", empty)
	}

	for i, b := range fcontent {
		if b != fcontentRead[i] {
			t.Error("Bytes at place", i, "don't match")
		}
	}

	err = DeleteFileRSC(host, fname, ringsz, dCert)
	if err != nil {
		fmt.Print(err.Error())
		t.Error(err)
	}
}
