package natTraverse

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

func (t *TraversalServer) Run() {
	t.targetMap = make(map[string]chan Message)                //udp分发消息用
	t.tonkenMap = make(map[string]chan holePunchingConnection) //tcp找到两个想建立连接的节点
	TCPMsgCh := make(chan Message, 10)                         //TCP To UDP
	t.targetMapLock = &sync.Mutex{}
	t.tonkenMapLock = &sync.Mutex{}
	rand.Seed(time.Now().UnixNano())
	go t.TestNATServer(TCPMsgCh)
	t.TCPListenServer(TCPMsgCh)
	// t.UDPListen()
	time.Sleep(time.Second * 1000)
}

type holePunchingConnection struct {
	TCPConn *net.TCPConn
	NAT     NATTypeINfo
}

type nodeType int

const (
	passive nodeType = iota
	active
)

// 给两个想打洞的节点返回的消息
type holePunchingNegotiationMsg struct {
	MyPublicAddr string
	RPublicAddr  string
	RNAT         NATTypeINfo //对方的NAT类型
	ServerPort   string
	MyType       nodeType //主动还是被动
}

func (h *holePunchingNegotiationMsg) unmarshal(data []byte) error {
	return json.Unmarshal(data, h)
}

func (t *TraversalServer) TCPListenServer(TCPMsgCh chan Message) {
	TCPladdr, err := net.ResolveTCPAddr("tcp4", t.ListenAddr)
	if err != nil {
		log.Println("resolve tcp addr error", err)
		return
	}
	tcpListener, err := net.ListenTCP("tcp4", TCPladdr)
	if err != nil {
		log.Println("listen tcp error", err)
		return
	}
	defer tcpListener.Close()
	fmt.Println("tcp listen on", tcpListener.Addr().String())
	for {
		tcpConn, err := tcpListener.AcceptTCP()
		if err != nil {
			log.Println("accept tcp error", err)
			continue
		}
		go t.handleTCPConn(tcpConn, TCPMsgCh)
	}
}

func (t *TraversalServer) handleTCPConn(tcpConn *net.TCPConn, TCPMsgCh chan Message) {
	msg, err := TCPReceiveMessage(tcpConn)
	if err != nil {
		if err == io.EOF {
			log.Println(tcpConn.RemoteAddr().String(), "tcp connection closed")
		} else {
			log.Println("tcp receive message error", err)
		}
		tcpConn.Close()
		return
	}
	msg.SrcPublicAddr = tcpConn.RemoteAddr().String()
	fmt.Println("handleTCPConn receive message:", tcpConn.RemoteAddr().String())

	switch msg.Type {
	case ProtocolChangeTest:
		TCPMsgCh <- msg //TCP To UDP
		fmt.Println("message send to UDP", msg)
	case Connection:
		err := t.handleConnection(tcpConn, msg)
		if err != nil {
			log.Println("handle connection error", err)
		}
		return
	default:
		log.Println("unknown message type", msg.Type)
	}
	tcpConn.Close()
}

func (t *TraversalServer) handleConnection(tcpConn *net.TCPConn, msg Message) error {
	//根据IdentityToken判断对方请求的打洞类型
	if len(msg.IdentityToken) < 3 || (msg.IdentityToken[len(msg.IdentityToken)-3:] != "UDP" && msg.IdentityToken[len(msg.IdentityToken)-3:] != "TCP") {
		info := "identity token error,not found holepunching type"
		log.Println(info)
		err := TCPSendMessage(tcpConn, Message{Type: ErrorResponse, ErrorInfo: info})
		if err != nil {
			log.Println("send error response error", err)
		}
		return fmt.Errorf(info)
	}
	fmt.Println(msg.IdentityToken)
	var natInfo NATTypeINfo
	err := json.Unmarshal(msg.Data, &natInfo)
	if err != nil {
		info := "identity token error,not found holepunching type"
		log.Println(info)
		err := TCPSendMessage(tcpConn, Message{Type: ErrorResponse, ErrorInfo: info})
		if err != nil {
			log.Println("send error response error", err)
		}
		return fmt.Errorf(info)
	}
	ch, ok := t.tonkenMap[msg.IdentityToken]
	if ok {
		ch <- holePunchingConnection{TCPConn: tcpConn, NAT: natInfo}
		return nil
	}
	defer func() {
		t.tonkenMapLock.Lock()
		delete(t.tonkenMap, msg.IdentityToken)
		t.tonkenMapLock.Unlock()
	}()
	var tcpConn2 *net.TCPConn
	t.tonkenMapLock.Lock()
	t.tonkenMap[msg.IdentityToken] = make(chan holePunchingConnection, 1)
	t.tonkenMapLock.Unlock()
	ch = t.tonkenMap[msg.IdentityToken]
	var hole holePunchingConnection
	select {
	case hole = <-ch:
		tcpConn2 = hole.TCPConn
		fmt.Println("holepunching object:", tcpConn.RemoteAddr().String(), tcpConn2.RemoteAddr().String())
	case <-time.After(time.Second * 30):
		info := "connection timeout,not found holepunching object"
		log.Println(info)
		err := TCPSendMessage(tcpConn, Message{Type: ErrorResponse, ErrorInfo: info})
		if err != nil {
			log.Println("send error response error", err)
		}
		return fmt.Errorf(info)
	}
	holeType := msg.IdentityToken[len(msg.IdentityToken)-3:]
	if holeType == "UDP" {
		err := handleUDPHolePunching(tcpConn, tcpConn2, natInfo, hole.NAT)
		if err != nil {
			log.Println("handleUDPHolePunching error", err)
			return err
		}
		return nil
	}
	if holeType == "TCP" {
		err := handleTCPHolePunching(tcpConn, tcpConn2, natInfo, hole.NAT)
		if err != nil {
			log.Println("handleTCPHolePunching error", err)
			return err
		}
		return nil
	}
	return nil
}

func handleUDPHolePunching(tcpConn1, tcpConn2 *net.TCPConn, natInfo1, natInfo2 NATTypeINfo) error {
	//创建两个随机端口的UDP连接，用可靠UDP协议包装
	tempUdpConn1, err := UDPRandListen()
	if err != nil {
		log.Println("listen udp error", err)
		return err
	}
	tempRudpConn1 := NewReliableUDP(tempUdpConn1)
	tempRudpConn1.SetGlobalReceive()

	tempUdpConn2, err := UDPRandListen()
	if err != nil {
		log.Println("listen udp error", err)
		return err
	}
	tempRudpConn2 := NewReliableUDP(tempUdpConn2)
	tempRudpConn2.SetGlobalReceive()

	randPort1 := tempUdpConn1.LocalAddr().(*net.UDPAddr).Port
	randPort2 := tempUdpConn2.LocalAddr().(*net.UDPAddr).Port
	//虽说是端口，但是这里是一个字符串，包含了ip和端口
	portCh1 := make(chan string, 1)
	portCh2 := make(chan string, 1)
	go func() {
		_, addr, err := RUDPReceiveAllMessage(tempRudpConn1, 0)
		if err != nil {
			log.Println("receive message error", err)
			return
		}
		portCh1 <- addr.String() //虽说是端口，但是这里是一个字符串，包含了ip和端口
		tempRudpConn1.Close()
	}()
	go func() {
		_, addr, err := RUDPReceiveAllMessage(tempRudpConn2, 0)
		if err != nil {
			log.Println("receive message error", err)
			return
		}
		portCh2 <- addr.String()
		tempRudpConn2.Close()
	}()
	//设置两个节点谁主动谁被动
	var conn1Isactive bool
	if natInfo1.NATType != Symmetric && natInfo2.NATType == Symmetric {
		conn1Isactive = false
	} else {
		conn1Isactive = true
	}
	//通知两个节点向我发送udp消息以此获取结点的公网端口
	msg1 := Message{
		Type: PunchingNegotiation,
	}
	holeMsg1 := holePunchingNegotiationMsg{
		MyPublicAddr: tcpConn1.RemoteAddr().String(),
		RPublicAddr:  tcpConn2.RemoteAddr().String(),
		RNAT:         natInfo2,
		ServerPort:   fmt.Sprintf("%d", randPort1),
	}
	if conn1Isactive {
		holeMsg1.MyType = active
	} else {
		holeMsg1.MyType = passive
	}
	data1, err := json.Marshal(holeMsg1)
	if err != nil {
		log.Println("marshal holepunching negotiation message error", err)
		return err
	}
	msg1.Data = data1

	// err = TCPSendMessage(tcpConn1, msg1)
	// if err != nil {
	// 	log.Println("send port negotiation message error", err)
	// 	return err
	// }
	msg2 := Message{
		Type: PunchingNegotiation,
	}
	holeMsg2 := holePunchingNegotiationMsg{
		MyPublicAddr: tcpConn2.RemoteAddr().String(),
		RPublicAddr:  tcpConn1.RemoteAddr().String(),
		RNAT:         natInfo1,
		ServerPort:   fmt.Sprintf("%d", randPort2),
	}
	if conn1Isactive {
		holeMsg2.MyType = passive
	} else {
		holeMsg2.MyType = active
	}
	data2, err := json.Marshal(holeMsg2)
	if err != nil {
		log.Println("marshal holepunching negotiation message error", err)
		return err
	}
	msg2.Data = data2
	//发送打洞协商消息,被动方先发
	//先给打洞方(被动方)发端口信息，防止洞还没打好连接请求就到了
	if conn1Isactive {
		err = TCPSendMessage(tcpConn2, msg2)
		if err != nil {
			log.Println("send port negotiation message error", err)
			return err
		}
		err = TCPSendMessage(tcpConn1, msg1)
		if err != nil {
			log.Println("send port negotiation message error", err)
			return err
		}
	} else {
		err = TCPSendMessage(tcpConn1, msg1)
		if err != nil {
			log.Println("send port negotiation message error", err)
			return err
		}
		err = TCPSendMessage(tcpConn2, msg2)
		if err != nil {
			log.Println("send port negotiation message error", err)
			return err
		}
	}
	//等待两个节点的端口信息
	var newRaddr1, newRaddr2 string
	for {
		select {
		case newRaddr1 = <-portCh1:
			log.Println("receive port1", newRaddr1)
		case newRaddr2 = <-portCh2:
			log.Println("receive port2", newRaddr2)
		case <-time.After(5 * time.Second):
			log.Println("receive port timeout")
			return errors.New("receive port timeout")
		}
		if newRaddr1 != "" && newRaddr2 != "" {
			break
		}
	}
	//通知两个节点对方的公网地址和端口，开始打洞
	msg1 = Message{
		Type: StartPunching,
		Data: []byte(newRaddr2),
	}
	msg2 = Message{
		Type: StartPunching,
		Data: []byte(newRaddr1),
	}
	if conn1Isactive {
		err = TCPSendMessage(tcpConn2, msg2)
		if err != nil {
			log.Println("send port negotiation message error", err)
			return err
		}
		err = TCPSendMessage(tcpConn1, msg1)
		if err != nil {
			log.Println("send port negotiation message error", err)
			return err
		}
	} else {
		err = TCPSendMessage(tcpConn1, msg1)
		if err != nil {
			log.Println("send port negotiation message error", err)
			return err
		}
		err = TCPSendMessage(tcpConn2, msg2)
		if err != nil {
			log.Println("send port negotiation message error", err)
			return err
		}
	}
	return nil
}

func handleTCPHolePunching(tcpConn1, tcpConn2 *net.TCPConn, natInfo1, natInfo2 NATTypeINfo) error {
	//创建两个随机端口的TCP监听
	tempTCPListener1, err := TCPRandListen()
	if err != nil {
		log.Println("create temp tcp listener error", err)
		return err
	}
	defer tempTCPListener1.Close()
	tempTCPListener2, err := TCPRandListen()
	if err != nil {
		log.Println("create temp tcp listener error", err)
		return err
	}
	defer tempTCPListener2.Close()
	randPort1 := tempTCPListener1.Addr().(*net.TCPAddr).Port
	randPort2 := tempTCPListener2.Addr().(*net.TCPAddr).Port
	//虽说是端口，但是这里是一个字符串，包含了ip和端口
	portCh1 := make(chan string, 1)
	portCh2 := make(chan string, 1)
	go func() {
		tcpConn, err := tempTCPListener1.AcceptTCP()
		if err != nil {
			log.Println("accept tcp connection error", err)
			return
		}
		portCh1 <- tcpConn.RemoteAddr().String()
		tcpConn.Close()
	}()
	go func() {
		tcpConn, err := tempTCPListener2.AcceptTCP()
		if err != nil {
			log.Println("accept tcp connection error", err)
			return
		}
		portCh2 <- tcpConn.RemoteAddr().String()
		tcpConn.Close()
	}()
	//设置两个节点谁主动谁被动
	var conn1Isactive bool
	if natInfo1.NATType != Symmetric && natInfo2.NATType == Symmetric {
		conn1Isactive = false
	} else {
		conn1Isactive = true
	}
	//通知两个节点向我连接tcp以此获取结点的公网端口
	msg1 := Message{
		Type: PunchingNegotiation,
	}
	holeMsg1 := holePunchingNegotiationMsg{
		MyPublicAddr: tcpConn1.RemoteAddr().String(),
		RPublicAddr:  tcpConn2.RemoteAddr().String(),
		RNAT:         natInfo2,
		ServerPort:   fmt.Sprintf("%d", randPort1),
	}
	if conn1Isactive {
		holeMsg1.MyType = active
	} else {
		holeMsg1.MyType = passive
	}
	// err = TCPSendMessage(tcpConn1, msg1)
	// if err != nil {
	// 	log.Println("send port negotiation message error", err)
	// 	return err
	// }
	msg2 := Message{
		Type: PunchingNegotiation,
	}
	holeMsg2 := holePunchingNegotiationMsg{
		MyPublicAddr: tcpConn2.RemoteAddr().String(),
		RPublicAddr:  tcpConn1.RemoteAddr().String(),
		RNAT:         natInfo1,
		ServerPort:   fmt.Sprintf("%d", randPort2),
	}
	if conn1Isactive {
		holeMsg2.MyType = passive
	} else {
		holeMsg2.MyType = active
	}
	//发送打洞协商消息,被动方先发
	//先给打洞方(被动方)发端口信息，防止洞还没打好连接请求就到了
	if conn1Isactive {
		err = TCPSendMessage(tcpConn2, msg2)
		if err != nil {
			log.Println("send port negotiation message error", err)
			return err
		}
		err = TCPSendMessage(tcpConn1, msg1)
		if err != nil {
			log.Println("send port negotiation message error", err)
			return err
		}
	} else {
		err = TCPSendMessage(tcpConn1, msg1)
		if err != nil {
			log.Println("send port negotiation message error", err)
			return err
		}
		err = TCPSendMessage(tcpConn2, msg2)
		if err != nil {
			log.Println("send port negotiation message error", err)
			return err
		}
	}
	//等待两个节点的端口信息
	var newRaddr1, newRaddr2 string
	for {
		select {
		case newRaddr1 = <-portCh1:
			log.Println("receive port1", newRaddr1)
		case newRaddr2 = <-portCh2:
			log.Println("receive port2", newRaddr2)
		case <-time.After(5 * time.Second):
			log.Println("receive port timeout")
			return errors.New("receive port timeout")
		}
		if newRaddr1 != "" && newRaddr2 != "" {
			break
		}
	}
	if newRaddr1[:strings.LastIndex(newRaddr1, ":")] != tcpConn1.RemoteAddr().String()[:strings.LastIndex(tcpConn1.RemoteAddr().String(), ":")] {
		log.Println("receive port1 error")
		fmt.Println(newRaddr1[:strings.LastIndex(newRaddr1, ":")], tcpConn1.RemoteAddr().String()[:strings.LastIndex(tcpConn1.RemoteAddr().String(), ":")])
		return errors.New("receive port1 error")
	}
	if newRaddr2[:strings.LastIndex(newRaddr2, ":")] != tcpConn2.RemoteAddr().String()[:strings.LastIndex(tcpConn2.RemoteAddr().String(), ":")] {
		log.Println("receive port2 error")
		fmt.Println(newRaddr2[:strings.LastIndex(newRaddr2, ":")], tcpConn2.RemoteAddr().String()[:strings.LastIndex(tcpConn2.RemoteAddr().String(), ":")])
		return errors.New("receive port2 error")
	}
	//通知两个节点对方的公网地址和端口，开始打洞
	msg1 = Message{
		Type: StartPunching,
		Data: []byte(newRaddr2),
	}
	msg2 = Message{
		Type: StartPunching,
		Data: []byte(newRaddr1),
	}
	if conn1Isactive {
		err = TCPSendMessage(tcpConn2, msg2)
		if err != nil {
			log.Println("send port negotiation message error", err)
			return err
		}
		err = TCPSendMessage(tcpConn1, msg1)
		if err != nil {
			log.Println("send port negotiation message error", err)
			return err
		}
	} else {
		err = TCPSendMessage(tcpConn1, msg1)
		if err != nil {
			log.Println("send port negotiation message error", err)
			return err
		}
		err = TCPSendMessage(tcpConn2, msg2)
		if err != nil {
			log.Println("send port negotiation message error", err)
			return err
		}
	}
	return nil
}

func (t *TraversalServer) TestNATServer(TCPMsgCh chan Message) {
	laddr, err := net.ResolveUDPAddr("udp4", t.ListenAddr)
	if err != nil {
		panic(err)
	}
	udpConn, err := net.ListenUDP("udp4", laddr)
	if err != nil {
		panic(err)
	}
	for {
		var msg Message
		receiveCh := make(chan Message, 1)
		go func() {
			msg, raddr, err := UDPReceiveMessage(udpConn, 0)
			if err != nil {
				log.Println("receive message error", err)
				return
			}
			msg.SrcPublicAddr = raddr.String()
			receiveCh <- msg
		}()
		select {
		case msg = <-receiveCh:
		case msg = <-TCPMsgCh:
		}
		fmt.Println("TestNATServer receive message:", msg.Type, msg.IdentityToken, string(msg.Data))
		switch msg.Type {
		case TestNatType:
			_, ok := t.targetMap[msg.IdentityToken]
			if ok {
				log.Println("receive duplicate message,identityToken:", msg.IdentityToken)
				continue
			}
			ch := make(chan Message, 2)
			t.targetMapLock.Lock()
			t.targetMap[msg.IdentityToken] = ch
			t.targetMapLock.Unlock()
			go t.handleTestNatType(udpConn, msg.SrcPublicAddr, msg.IdentityToken, ch)
		default:
			ch := t.targetMap[msg.IdentityToken]
			if ch != nil {
				ch <- msg
			} else {
				log.Println("receive timeout message:", msg.Type, msg.IdentityToken, string(msg.Data))
			}
		}
	}
}

func (t *TraversalServer) handleTestNatType(udpConn *net.UDPConn, raddr string, identityToken string, UDPMsgCh chan Message) {
	defer func() {
		t.targetMapLock.Lock()
		delete(t.targetMap, identityToken)
		t.targetMapLock.Unlock()
	}()
	var msg Message
	msg.Type = ACK
	err := UDPSendMessage(udpConn, raddr, msg)
	if err != nil {
		log.Println("send message error", err)
		return
	}
	fmt.Println("send ack to", raddr)
	tempUdpConn, err := UDPRandListen()
	if err != nil {
		log.Println("listen udp error", err)
		return
	}
	defer tempUdpConn.Close()
	randPort := tempUdpConn.LocalAddr().String()[strings.LastIndex(tempUdpConn.LocalAddr().String(), ":")+1:]
	fmt.Println("rand port:", randPort)
	msg.Type = PortNegotiation
	msg.Data = []byte(fmt.Sprint(randPort))
	err = UDPSendMessage(udpConn, raddr, msg)
	if err != nil {
		log.Println("send message error", err)
		return
	}
	fmt.Println("send port negotiation to", raddr)
	msg, newRAddr, err := UDPReceiveMessage(tempUdpConn, 2*time.Second)
	if err != nil {
		log.Println("receive message error", err)
		return
	}
	fmt.Println("receive message from", newRAddr.String())
	if msg.Type != PortNegotiationResponse {
		log.Println("receive message error", msg.Type, msg.IdentityToken, string(msg.Data))
		return
	}
	natInfo := NATTypeINfo{}
	FinallType := UnKnown
	var changeRule PortChange
	if newRAddr.String() != raddr {
		//Symmetric NAT
		log.Println("Symmetric NAT")
		FinallType = Symmetric
		raddrPort, err := strconv.Atoi(raddr[strings.LastIndex(raddr, ":")+1:])
		if err != nil {
			log.Println("strconv.Atoi error", err)
			return
		}
		//端口变化范围
		portRange := math.Abs(float64(raddrPort - newRAddr.Port))
		if portRange <= 100 {
			changeRule = Linear
		} else {
			changeRule = UnKnownRule
		}
	} else {
		//非对称NAT
		tempUdpConn, err = UDPRandListen()
		if err != nil {
			log.Println("listen udp error", err)
			return
		}
		defer tempUdpConn.Close()
		msg2 := Message{
			Type: ServerPortChangeTest,
		}
		err = UDPSendMessage(udpConn, raddr, msg2)
		if err != nil {
			log.Println("send message error", err)
			return
		}
		fmt.Println("send server port change test to", raddr)
		var tempMsg Message
		var ok bool = true
		for ok {
			select {
			case msg = <-UDPMsgCh:
				log.Println("receive message", msg.Type, msg.IdentityToken, string(msg.Data))
				if msg.Type == ServerPortChangeTestResponse {
					FinallType = FullOrRestrictedCone
				} else if msg.Type == ProtocolChangeTest {
					tempMsg = msg
				} else {
					log.Println("unexpected message", msg.Type, msg.IdentityToken, string(msg.Data))
				}
			case <-time.After(time.Second * 2):
				log.Println("receive message timeout")
				FinallType = PortRestrictedCone
				if tempMsg.SrcPublicAddr != "" {
					UDPMsgCh <- tempMsg
				}
				ok = false
			}
		}
	}
	var ok bool = true
	for ok {
		select {
		case msg = <-UDPMsgCh:
			log.Println("receive message", msg.Type, msg.IdentityToken, string(msg.Data))
			if msg.Type == ServerPortChangeTestResponse {
				FinallType = FullOrRestrictedCone
			} else if msg.Type == ProtocolChangeTest {
				if raddr == msg.SrcPublicAddr {
					natInfo.PortInfluencedByProtocol = false
				} else {
					natInfo.PortInfluencedByProtocol = true
				}
				ok = false
			} else {
				log.Println("unexpected message", msg.Type, msg.IdentityToken, string(msg.Data))
			}
		case <-time.After(time.Second * 10):
			log.Println("receive message timeout,unexpected case")
			natInfo.PortInfluencedByProtocol = true
			ok = false
		}
	}
	natInfo.NATType = FinallType
	if FinallType == Symmetric {
		natInfo.PortChangeRule = changeRule
	}
	data, err := json.Marshal(natInfo)
	if err != nil {
		log.Println("marshal nat info error", err)
		return
	}
	finnalMsg := Message{
		Type:          EndResult,
		IdentityToken: msg.IdentityToken,
		Data:          data,
	}
	err = UDPSendMessage(udpConn, raddr, finnalMsg)
	if err != nil {
		log.Println("send message error", err)
		return
	}
	fmt.Println("send end result to", raddr)
	time.Sleep(time.Second * 2000)
}
