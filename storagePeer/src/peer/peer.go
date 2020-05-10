package peer

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"math"
	"net"
	"os"

	"github.com/klauspost/reedsolomon"
	"google.golang.org/grpc"

	"fmt"
	"storagePeer/src/dht"
	"time"
)

////////
// Data structures
////////

// Peer is the peer struct
type Peer struct {
	ownIP string
	ring  *dht.RingNode
	Errs  chan error
}

////////
// Local functions
////////

// NewPeer creates new peer
func NewPeer(ownIP string, maxNodes uint64, existingIP string) *Peer {

	p := Peer{ownIP: ownIP, ring: dht.NewRingNode(ownIP, maxNodes), Errs: make(chan error, 1)}

	p.start()

	// Join the network. Build finger table and adapt the other ones.
	p.ring.Join(existingIP)

	return &p
}

// MarshalJSON converts peer to JSON
func (p *Peer) MarshalJSON() ([]byte, error) {

	return json.Marshal(struct {
		OwnIP string
		Ring  *dht.RingNode
	}{
		OwnIP: p.ownIP,
		Ring:  p.ring,
	})
}

// Start starts gRPC server for peer in a seperate go routine
func (p *Peer) start() {
	// Configure listening

	lis, err := net.Listen("tcp", p.ownIP)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	// create a gRPC server object
	grpcServer := grpc.NewServer()

	// attach services to handler object
	RegisterPeerServiceServer(grpcServer, p)
	dht.RegisterRingServiceServer(grpcServer, p.ring)

	// Start listening in a separate go routine
	go func() {
		p.Errs <- grpcServer.Serve(lis)
		close(p.Errs)
	}()
}

// Connect connects to peer with specified IP
func Connect(targetIP string) (*grpc.ClientConn, PeerServiceClient, error) {
	conn, err := grpc.Dial(targetIP, grpc.WithInsecure())

	if err != nil {
		for {
			conn, err = grpc.Dial(targetIP, grpc.WithInsecure())
			if err != nil {
				fmt.Println(err)
				fmt.Println("Couldn't connect to node", targetIP)
			} else {
				break
			}

			time.Sleep(time.Second * 1)
		}
	}

	cl := NewPeerServiceClient(conn)
	return conn, cl, nil
}

////////
// Send and recieve files
////////

func connectAndFindSuccessor(ringIP string, id uint64) (string, error) {

	someConn, somePeer, err := Connect(ringIP)
	if err != nil {
		return "", err
	}
	defer someConn.Close()

	ip := ""
	for {
		succReply, err := somePeer.FindSuccessorInRing(context.Background(), &FindSuccRequest{Id: id})

		if err == nil {
			ip = succReply.Ip
			break
		} else {
			fmt.Println(err.Error())
			fmt.Println("Couldn't fetch ip")
			time.Sleep(time.Second * 1)
		}
	}

	fmt.Printf("Ring has answered with ip %s\n", ip)
	return ip, nil
}

const chunksz = 8

// SendFile sends file to the target IP
func SendFile(targetIP string, fname string, fcontent []byte) error {

	conn, cl, err := Connect(targetIP)
	if err != nil {
		return err
	}
	defer conn.Close()

	fmt.Printf("Opening write stream to %s...", targetIP)
	// Stream to write
	wstream, err := cl.Write(context.Background())
	for err != nil {
		fmt.Println(err.Error())
		fmt.Println("Couldn't initialize remote write stream")
		time.Sleep(time.Second * 1)

		wstream, err = cl.Write(context.Background())
	}

	fmt.Printf("Sending filename %s...", fname)
	// Send filename
	err = wstream.Send(&WriteRequest{Name: fname})

	for err != nil {
		fmt.Println(err.Error())
		fmt.Println("Couldn't send filename")
		time.Sleep(time.Second * 1)
		err = wstream.Send(&WriteRequest{Name: fname})
	}

	chunkSize := chunksz
	chunkAmnt := int(math.Ceil(float64(len(fcontent)) / float64(chunkSize)))

	fmt.Println("Writing to file, total chunks:", chunkAmnt)
	for i := 0; i < chunkAmnt; i++ {

		curChunk := fcontent[i*chunkSize:]
		if len(curChunk) > chunkSize {
			fmt.Println()
			curChunk = curChunk[:chunkSize]
		}

		err := wstream.Send(&WriteRequest{Data: curChunk})

		for err != nil {
			fmt.Println(err.Error())
			fmt.Printf("Unable to send chunk #%d", i)
			time.Sleep(time.Second * 1)
			err = wstream.Send(&WriteRequest{Name: fname})
		}
	}

	reply, err := wstream.CloseAndRecv()
	fmt.Printf("Finished writing %d bytes", reply.Written)
	return err
}

// UploadFile uploads file to the successor of an id. ringIP - ip of someone on the ring
func UploadFile(ringIP string, fname string, ringsz uint64, fcontent []byte) error {

	id := dht.Hash([]byte(fname), ringsz)
	targetIP, err := connectAndFindSuccessor(ringIP, id)
	if err != nil {
		return err
	}

	return SendFile(targetIP, fname, fcontent)
}

// RecvFile recieves file w/ filename=fname, from node targetIP - returns how much empty space is at the end (negative, if buffer is too small)
func RecvFile(targetIP string, fname string, fcontent []byte) (int, error) {

	conn, peer, err := Connect(targetIP)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	rstream, err := peer.Read(context.Background(), &ReadRequest{Name: fname, ChunkSize: chunksz})
	if err != nil {
		return 0, err
	}

	readReply, err := rstream.Recv()
	if !readReply.Exists {
		return 0, os.ErrNotExist
	}

	contentSlice := fcontent[:]
	bufferSmall := false
	emptySpace := len(fcontent)

	for {
		readReply, err := rstream.Recv()

		if err == io.EOF {
			break
		}

		if err != nil {
			return 0, err
		}

		if !bufferSmall && int(readReply.Size) > emptySpace {
			for i := range contentSlice {
				contentSlice[i] = readReply.Data[i]
			}

			emptySpace = 0
			bufferSmall = true
		}

		if !bufferSmall {
			for i := 0; i < int(readReply.Size); i++ {
				contentSlice[i] = readReply.Data[i]
			}
			contentSlice = contentSlice[readReply.Size:]
		}

		emptySpace -= int(readReply.Size)
	}

	return emptySpace, nil
}

// DownloadFile downloads file from the corresponding node
func DownloadFile(ringIP string, fname string, ringsz uint64, fcontent []byte) (int, error) {
	id := dht.Hash([]byte(fname), ringsz)
	targetIP, err := connectAndFindSuccessor(ringIP, id)

	if err != nil {
		return 0, err
	}

	return RecvFile(targetIP, fname, fcontent)
}

////////
// Reed-Solomon
////////

const dataRSC = 8
const parityRSC = 2

func getShardName(fname string, number int) string {
	return fmt.Sprintf("%s_rep%d", fname, number)
}

// UploadFileRSC - like UploadFile but with Reed-Solomon erasure coding
func UploadFileRSC(ringIP string, fname string, ringsz uint64, fcontent []byte) error {
	enc, err := reedsolomon.New(dataRSC, parityRSC)
	if err != nil {
		return err
	}

	N := len(fcontent)
	data := make([][]byte, dataRSC+parityRSC)
	shardLen := int(math.Ceil(float64(N) / float64(dataRSC)))
	fcontentSlice := fcontent

	for i := 0; i < dataRSC-1; i++ {
		data[i] = fcontentSlice[:shardLen]
		fcontentSlice = fcontentSlice[shardLen:]
	}

	data[dataRSC-1] = fcontentSlice
	for i := 0; i < parityRSC; i++ {
		data[dataRSC+i] = make([]byte, shardLen)
	}

	err = enc.Encode(data)
	if err != nil {
		return err
	}

	for i, s := range data {
		err := UploadFile(ringIP, getShardName(fname, i), ringsz, s)
		if err != nil {
			return err
		}
	}

	return nil
}

// DownloadFileRSC downloads file using Reed Solomon Codes
func DownloadFileRSC(ringIP string, fname string, ringsz uint64, fcontent []byte) (int, error) {
	enc, _ := reedsolomon.New(dataRSC, parityRSC)

	shards := make([][]byte, dataRSC+parityRSC)
	var shardlen int
	maxshardlen := int(math.Floor(float64(len(fcontent)) / float64(dataRSC)))
	firstShard := make([]byte, maxshardlen)
	shardnum := 0
	nilshards := make(map[int]bool)

	for {
		if shardnum > parityRSC {
			return 0, fmt.Errorf("Too many corrupt files, can't recover")
		}

		empty, err := DownloadFile(ringIP, getShardName(fname, shardnum), ringsz, firstShard)
		if !os.IsNotExist(err) {
			shardlen = maxshardlen - empty
			if (shardnum+1)*shardlen > len(fcontent) {
				return 0, fmt.Errorf("Not enough space in buffer")
			}

			copy(fcontent[shardnum*shardlen:], firstShard)
			//shardnum++
			break
		}

		shards[shardnum] = nil
		nilshards[shardnum] = true
		shardnum++
	}

	totalEmpty := 0

	for ; shardnum < dataRSC+parityRSC; shardnum++ {
		if shardnum < dataRSC {
			shards[shardnum] = fcontent[shardlen*shardnum : shardlen*(shardnum+1)]
		} else {
			shards[shardnum] = make([]byte, shardlen)
		}

		empty, err := DownloadFile(ringIP, fmt.Sprintf("%s_rep%d", fname, shardnum), ringsz, shards[shardnum])
		if os.IsNotExist(err) {
			shards[shardnum] = nil
			nilshards[shardnum] = true
			continue
		}

		if err != nil {
			return 0, err
		}

		totalEmpty += empty
	}

	err := enc.ReconstructData(shards)
	if err != nil {
		return 0, err
	}

	for n := range nilshards {
		copy(fcontent[shardlen*n:shardlen*(n+1)], shards[n])
	}

	return totalEmpty, nil
}

////////
// Remote calls
////////

// Ping generates response to a Ping request
func (p *Peer) Ping(ctx context.Context, in *PingMessage) (*PingMessage, error) {
	log.Printf("Receive message %t", in.Ok)
	return &PingMessage{Ok: true}, nil
}

// FindSuccessorInRing finds id's successor in p's ring
func (p *Peer) FindSuccessorInRing(ctx context.Context, r *FindSuccRequest) (*FindSuccReply, error) {
	ip, err := p.ring.FindSuccessor(r.Id)
	return &FindSuccReply{Ip: ip}, err
}

// Read & Write ----------------------

// Read reads the content of a specified file
func (p *Peer) Read(r *ReadRequest, stream PeerService_ReadServer) error {

	f, err := os.Open(r.Name)
	if os.IsNotExist(err) {
		stream.Send(&ReadReply{Exists: false})
		return nil
	}

	stream.Send(&ReadReply{Exists: true})

	if err != nil {
		log.Fatal(err)

		return err
	}

	defer f.Close()

	reader := bufio.NewReader(f)
	b := make([]byte, r.ChunkSize)

	for {
		n, readErr := reader.Read(b)

		if readErr == io.EOF {
			break
		}

		if readErr != nil {
			return err
		}

		if err := stream.Send(&ReadReply{Data: b, Size: int64(n)}); err != nil {
			return err
		}
	}

	return nil
}

// Write writes the content of the request r onto the disk
func (p *Peer) Write(stream PeerService_WriteServer) error {

	writeInfo, err := stream.Recv()

	if err != nil {
		return err
	}

	f, err := os.Create(writeInfo.Name)
	defer f.Close()

	if err != nil {
		return err
	}

	writer := bufio.NewWriter(f)

	n, err := writer.Write(writeInfo.Data)

	if err != nil {
		return err
	}

	written := int64(n)

	for {
		toWrite, readErr := stream.Recv()

		if readErr == io.EOF {
			if err = writer.Flush(); err != nil {
				return err
			}

			return stream.SendAndClose(&WriteReply{Written: int64(written)})
		}

		if readErr != nil {
			return readErr
		}

		n, err := writer.Write(toWrite.Data)

		if err != nil {
			return err
		}

		written += int64(n)
	}
}
