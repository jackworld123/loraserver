package gateway

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"sync"
	"text/template"
	"time"

	"github.com/brocaar/loraserver/api/gw"
	"github.com/brocaar/loraserver/internal/backend"
	"github.com/brocaar/loraserver/internal/config"
	"github.com/brocaar/lorawan"
	"github.com/eclipse/paho.mqtt.golang"
	"github.com/garyburd/redigo/redis"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

const uplinkLockTTL = time.Millisecond * 500
const statsLockTTL = time.Millisecond * 500

// Backend implements a MQTT pub-sub backend.
type Backend struct {
	conn            mqtt.Client
	rxPacketChan    chan gw.RXPacket
	statsPacketChan chan gw.GatewayStatsPacket
	wg              sync.WaitGroup

	uplinkTopic      string
	statsTopic       string
	ackTopic         string
	downlinkTemplate *template.Template
}

// NewBackend creates a new Backend.
func NewBackend(server, username, password, cafile, certFile, certKeyFile, uplinkTopic, downlinkTopic, statsTopic, ackTopic string) (backend.Gateway, error) {
	var err error
	b := Backend{
		rxPacketChan:    make(chan gw.RXPacket),
		statsPacketChan: make(chan gw.GatewayStatsPacket),
		uplinkTopic:     uplinkTopic,
		statsTopic:      statsTopic,
		ackTopic:        ackTopic,
	}

	b.downlinkTemplate, err = template.New("downlink").Parse(downlinkTopic)
	if err != nil {
		return nil, errors.Wrap(err, "parse downlink template error")
	}

	opts := mqtt.NewClientOptions()
	opts.AddBroker(server)
	opts.SetUsername(username)
	opts.SetPassword(password)
	opts.SetOnConnectHandler(b.onConnected)
	opts.SetConnectionLostHandler(b.onConnectionLost)

	tlsconfig, err := newTLSConfig(cafile, certFile, certKeyFile)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"ca_cert":  cafile,
			"tls_cert": certFile,
			"tls_key":  certKeyFile,
		}).Fatal("error loading mqtt certificate files")
	}
	if tlsconfig != nil {
		opts.SetTLSConfig(tlsconfig)
	}

	log.WithField("server", server).Info("backend/gateway: connecting to mqtt broker")
	b.conn = mqtt.NewClient(opts)
	for {
		if token := b.conn.Connect(); token.Wait() && token.Error() != nil {
			log.Errorf("backend/gateway: connecting to mqtt broker failed, will retry in 2s: %s", token.Error())
			time.Sleep(2 * time.Second)
		} else {
			break
		}
	}

	return &b, nil
}

func newTLSConfig(cafile, certFile, certKeyFile string) (*tls.Config, error) {
	// Here are three valid options:
	//   - Only CA
	//   - TLS cert + key
	//   - CA, TLS cert + key

	if cafile == "" && certFile == "" && certKeyFile == "" {
		log.Info("backend/gateway: TLS config is empty")
		return nil, nil
	}

	tlsConfig := &tls.Config{}

	// Import trusted certificates from CAfile.pem.
	if cafile != "" {
		cacert, err := ioutil.ReadFile(cafile)
		if err != nil {
			log.Errorf("backend: couldn't load cafile: %s", err)
			return nil, err
		}
		certpool := x509.NewCertPool()
		certpool.AppendCertsFromPEM(cacert)

		tlsConfig.RootCAs = certpool // RootCAs = certs used to verify server cert.
	}

	// Import certificate and the key
	if certFile != "" && certKeyFile != "" {
		kp, err := tls.LoadX509KeyPair(certFile, certKeyFile)
		if err != nil {
			log.Errorf("backend: couldn't load MQTT TLS key pair: %s", err)
			return nil, err
		}
		tlsConfig.Certificates = []tls.Certificate{kp}
	}

	return tlsConfig, nil
}

// Close closes the backend.
// Note that this closes the backend one-way (gateway to backend).
// This makes it possible to perform a graceful shutdown (e.g. when there are
// still packets to send back to the gateway).
func (b *Backend) Close() error {
	log.Info("backend/gateway: closing backend")

	log.WithField("topic", b.uplinkTopic).Info("backend/gateway: unsubscribing from rx topic")
	if token := b.conn.Unsubscribe(b.uplinkTopic); token.Wait() && token.Error() != nil {
		return fmt.Errorf("backend/gateway: unsubscribe from %s error: %s", b.uplinkTopic, token.Error())
	}
	log.WithField("topic", b.statsTopic).Info("backend/gateway: unsubscribing from stats topic")
	if token := b.conn.Unsubscribe(b.statsTopic); token.Wait() && token.Error() != nil {
		return fmt.Errorf("backend/gateway: unsubscribe from %s error: %s", b.statsTopic, token.Error())
	}
	log.Info("backend/gateway: handling last messages")
	b.wg.Wait()
	close(b.rxPacketChan)
	close(b.statsPacketChan)
	return nil
}

// RXPacketChan returns the RXPacket channel.
func (b *Backend) RXPacketChan() chan gw.RXPacket {
	return b.rxPacketChan
}

// StatsPacketChan returns the gateway stats channel.
func (b *Backend) StatsPacketChan() chan gw.GatewayStatsPacket {
	return b.statsPacketChan
}

// SendTXPacket sends the given TXPacket to the gateway.
func (b *Backend) SendTXPacket(txPacket gw.TXPacket) error {
	phyB, err := txPacket.PHYPayload.MarshalBinary()
	if err != nil {
		return errors.Wrap(err, "marshal binary error")
	}
	bb, err := json.Marshal(gw.TXPacketBytes{
		Token:      txPacket.Token,
		TXInfo:     txPacket.TXInfo,
		PHYPayload: phyB,
	})
	if err != nil {
		return fmt.Errorf("backend/gateway: tx packet marshal error: %s", err)
	}

	topic := bytes.NewBuffer(nil)
	if err := b.downlinkTemplate.Execute(topic, struct{ MAC lorawan.EUI64 }{txPacket.TXInfo.MAC}); err != nil {
		return errors.Wrap(err, "execute uplink template error")
	}
	log.WithField("topic", topic.String()).Info("backend/gateway: publishing tx packet")

	if token := b.conn.Publish(topic.String(), 0, false, bb); token.Wait() && token.Error() != nil {
		return fmt.Errorf("backend/gateway: publish tx packet failed: %s", token.Error())
	}
	return nil
}

func (b *Backend) rxPacketHandler(c mqtt.Client, msg mqtt.Message) {
	b.wg.Add(1)
	defer b.wg.Done()

	log.Info("backend/gateway: rx packet received")

	var phy lorawan.PHYPayload
	var rxPacketBytes gw.RXPacketBytes
	if err := json.Unmarshal(msg.Payload(), &rxPacketBytes); err != nil {
		log.WithFields(log.Fields{
			"data_base64": base64.StdEncoding.EncodeToString(msg.Payload()),
		}).Errorf("backend/gateway: unmarshal rx packet error: %s", err)
		return
	}

	if err := phy.UnmarshalBinary(rxPacketBytes.PHYPayload); err != nil {
		log.WithFields(log.Fields{
			"data_base64": base64.StdEncoding.EncodeToString(msg.Payload()),
		}).Errorf("backend/gateway: unmarshal phypayload error: %s", err)
	}

	// Since with MQTT all subscribers will receive the uplink messages sent
	// by all the gatewyas, the first instance receiving the message must lock it,
	// so that other instances can ignore the same message (from the same gw).
	// As an unique id, the gw mac + base64 encoded payload is used. This is because
	// we can't trust any of the data, as the MIC hasn't been validated yet.
	strB, err := phy.MarshalText()
	if err != nil {
		log.Errorf("backend/gateway: marshal text error: %s", err)
	}
	key := fmt.Sprintf("lora:ns:uplink:lock:%s:%s", rxPacketBytes.RXInfo.MAC, string(strB))
	redisConn := config.C.Redis.Pool.Get()
	defer redisConn.Close()

	_, err = redis.String(redisConn.Do("SET", key, "lock", "PX", int64(uplinkLockTTL/time.Millisecond), "NX"))
	if err != nil {
		if err == redis.ErrNil {
			// the payload is already being processed by an other instance
			return
		}
		log.Errorf("backend/gateway: acquire uplink payload lock error: %s", err)
		return
	}

	b.rxPacketChan <- gw.RXPacket{
		RXInfo:     rxPacketBytes.RXInfo,
		PHYPayload: phy,
	}
}

func (b *Backend) statsPacketHandler(c mqtt.Client, msg mqtt.Message) {
	b.wg.Add(1)
	defer b.wg.Done()

	var statsPacket gw.GatewayStatsPacket
	if err := json.Unmarshal(msg.Payload(), &statsPacket); err != nil {
		log.WithFields(log.Fields{
			"data_base64": base64.StdEncoding.EncodeToString(msg.Payload()),
		}).Errorf("backend/gateway: unmarshal stats packet error: %s", err)
		return
	}

	// Since with MQTT all subscribers will receive the uplink messages sent
	// by all the gatewyas, the first instance receiving the message must lock it,
	// so that other instances can ignore the same message (from the same gw).
	// As an unique id, the gw mac + base64 encoded payload is used. This is because
	// we can't trust any of the data, as the MIC hasn't been validated yet.
	key := fmt.Sprintf("lora:ns:stats:lock:%s", statsPacket.MAC)
	redisConn := config.C.Redis.Pool.Get()
	defer redisConn.Close()

	_, err := redis.String(redisConn.Do("SET", key, "lock", "PX", int64(statsLockTTL/time.Millisecond), "NX"))
	if err != nil {
		if err == redis.ErrNil {
			// the payload is already being processed by an other instance
			return
		}
		log.Errorf("backend/gateway: acquire stats lock error: %s", err)
		return
	}

	log.WithField("mac", statsPacket.MAC).Info("backend/gateway: gateway stats packet received")
	b.statsPacketChan <- statsPacket
}

func (b *Backend) onConnected(c mqtt.Client) {
	log.Info("backend/gateway: connected to mqtt server")

	for {
		log.WithField("topic", b.uplinkTopic).Info("backend/gateway: subscribing to rx topic")
		if token := b.conn.Subscribe(b.uplinkTopic, 0, b.rxPacketHandler); token.Wait() && token.Error() != nil {
			log.WithField("topic", b.uplinkTopic).Errorf("backend/gateway: subscribe error: %s", token.Error())
			time.Sleep(time.Second)
			continue
		}
		break
	}

	for {
		log.WithField("topic", b.statsTopic).Info("backend/gateway: subscribing to stats topic")
		if token := b.conn.Subscribe(b.statsTopic, 0, b.statsPacketHandler); token.Wait() && token.Error() != nil {
			log.WithField("topic", b.statsTopic).Errorf("backend/gateway: subscribe error: %s", token.Error())
			time.Sleep(time.Second)
			continue
		}
		break
	}
}

func (b *Backend) onConnectionLost(c mqtt.Client, reason error) {
	log.Errorf("backend/gateway: mqtt connection error: %s", reason)
}
