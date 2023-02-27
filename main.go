package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/hkontrol/hkontroller"
)

const topicRequest = "/hkontroller/request"
const topicResponse = "/hkontroller/response"
const topicDeviceEvents = "/hkontroller/device_events"
const topicCharacteristicEvents = "/hkontroller/characteristic_events"

type PairRequestPayload struct {
	Device *string `json:"device"`
	Pin    *string `json:"pin"`
}
type UnpairRequestPayload struct {
	Device *string `json:"device"`
}
type VerifyRequestPayload struct {
	Device *string `json:"device"`
}
type GetDeviceInfoRequest struct {
	Device *string `json:"device"`
}
type GetAccessoriesRequest struct {
	Device *string `json:"device"`
}
type GetAccessoryInfoRequest struct {
	Device *string `json:"device"`
	Aid    *int    `json:"aid"`
}
type GetCharacteristicRequest struct {
	Device *string `json:"device"`
	Aid    *int    `json:"aid"`
	Iid    *int    `json:"iid"`
}
type SubscribeToCharacteristicRequest struct {
	Device *string `json:"device"`
	Aid    *int    `json:"aid"`
	Iid    *int    `json:"iid"`
}
type PutCharacteristicRequest struct {
	Device *string     `json:"device"`
	Aid    *int        `json:"aid"`
	Iid    *int        `json:"iid"`
	Value  interface{} `json:"value"`
}
type Request struct {
	Id      string          `json:"request_id"`
	Action  string          `json:"action"`
	Payload json.RawMessage `json:"payload"`
}
type Response struct {
	Id      string      `json:"response_id"`
	Error   string      `json:"error"`
	Payload interface{} `json:"payload"`
}

type Event struct {
	Event   string      `json:"event"`
	Payload interface{} `json:"payload"`
}

type MqttHkontroller struct {
	mq              mqtt.Client
	hk              *hkontroller.Controller
	cancelDiscovery context.CancelFunc
	qos             byte
}

type Device struct {
	Name           string              `json:"name"`
	Discovered     bool                `json:"discovered"`
	Paired         bool                `json:"paired"`
	Verified       bool                `json:"verified"`
	Pairing        hkontroller.Pairing `json:"pairing"`
	DnsServiceName string              `json:"dns_service_name"`
	Txt            map[string]string   `json:"txt"`
}

func convertDevice(d *hkontroller.Device) Device {
	return Device{
		Name:           d.Name,
		Discovered:     d.IsDiscovered(),
		Paired:         d.IsPaired(),
		Verified:       d.IsVerified(),
		Pairing:        d.GetPairingInfo(),
		Txt:            d.GetDnssdEntry().Text,
		DnsServiceName: d.GetDnssdEntry().ServiceInstanceName(),
	}
}
func convertDeviceList(devs []*hkontroller.Device) []Device {
	res := []Device{}
	for _, d := range devs {
		res = append(res, convertDevice(d))
	}
	return res
}

func (c *MqttHkontroller) publishDeviceEvent(event *Event) {
	jj, err := json.Marshal(event)
	if err != nil {
		fmt.Println("error marshaling event: ", err)
		return
	}
	c.mq.Publish(topicDeviceEvents, c.qos, false, jj)
}

func (c *MqttHkontroller) StartDiscovery() {
	ctx, cancel := context.WithCancel(context.Background())
	c.cancelDiscovery = cancel
	disco, lost := c.hk.StartDiscoveryWithContext(ctx)

	verify := func(d *hkontroller.Device) {
		err := d.PairVerify()
		if err != nil {
			fmt.Println("pair-verify err: ", err)
			return
		}
		fmt.Println("should be connected now")
	}

	go func() {
		for d := range disco {
			if d.IsPaired() {
				go verify(d)
			}

			// catch events and pass to receiver
			go func() {
				cc := convertDevice(d)
				ev := Event{
					Event:   "discovered",
					Payload: cc,
				}
				c.publishDeviceEvent(&ev)
			}()

			go func(dd *hkontroller.Device) {
				for range dd.OnPaired() {
					cc := convertDevice(dd)
					ev := Event{
						Event:   "paired",
						Payload: cc,
					}
					c.publishDeviceEvent(&ev)
				}
			}(d)
			go func(dd *hkontroller.Device) {
				for range dd.OnUnpaired() {
					cc := convertDevice(dd)
					ev := Event{
						Event:   "unpaired",
						Payload: cc,
					}
					c.publishDeviceEvent(&ev)
				}
			}(d)
			go func(dd *hkontroller.Device) {
				for range dd.OnVerified() {
					cc := convertDevice(dd)
					ev := Event{
						Event:   "verified",
						Payload: cc,
					}
					c.publishDeviceEvent(&ev)
				}
			}(d)
			go func(dd *hkontroller.Device) {
				for range dd.OnClose() {
					cc := convertDevice(dd)
					ev := Event{
						Event:   "closed",
						Payload: cc,
					}
					c.publishDeviceEvent(&ev)
				}
			}(d)
		}
	}()

	go func() {
		for d := range lost {
			cc := convertDevice(d)
			ev := Event{
				Event:   "lost",
				Payload: cc,
			}
			c.publishDeviceEvent(&ev)
		}
	}()
}

func (c *MqttHkontroller) PairDevice(req *Request) *Response {
	jj, err := req.Payload.MarshalJSON()
	if err != nil {
		return responseError(req, err)
	}
	var pairRequest PairRequestPayload
	err = json.Unmarshal(jj, &pairRequest)
	if err != nil || pairRequest.Device == nil || pairRequest.Pin == nil {
		return responseError(req, errors.New("wrong pair request payload. should be {device: string, pin: string}"))
	}
	dd := c.hk.GetDevice(*pairRequest.Device)
	if dd == nil {
		return responseError(req, errors.New("device not found"))
	}
	err = dd.PairSetup(*pairRequest.Pin)
	if err != nil {
		return responseError(req, err)
	}

	return &Response{
		Id:      req.Id,
		Payload: "success",
	}
}

func (c *MqttHkontroller) VerifyDevice(req *Request) *Response {
	jj, err := req.Payload.MarshalJSON()
	if err != nil {
		return responseError(req, err)
	}
	var verifyRequest VerifyRequestPayload
	err = json.Unmarshal(jj, &verifyRequest)
	if err != nil {
		return responseError(req, err)
	}
	if verifyRequest.Device == nil {
		return responseError(req, errors.New("wrong verify request payload. should be {device: string}"))
	}
	dd := c.hk.GetDevice(*verifyRequest.Device)
	if dd == nil {
		return responseError(req, errors.New("device not found"))
	}
	err = dd.PairVerify()
	if err != nil {
		return responseError(req, err)
	}

	return &Response{
		Id:      req.Id,
		Payload: "success",
	}
}

func (c *MqttHkontroller) UnpairDevice(req *Request) *Response {
	jj, err := req.Payload.MarshalJSON()
	if err != nil {
		return responseError(req, err)
	}
	var unpairRequest UnpairRequestPayload
	err = json.Unmarshal(jj, &unpairRequest)
	if err != nil || unpairRequest.Device == nil {
		return responseError(req, errors.New("wrong unpair request payload. should be {device: string}"))
	}
	dd := c.hk.GetDevice(*unpairRequest.Device)
	if dd == nil {
		return responseError(req, errors.New("device not found"))
	}
	err = dd.Unpair()
	if err != nil {
		return responseError(req, err)
	}

	return &Response{
		Id:      req.Id,
		Payload: "success",
	}
}

func (c *MqttHkontroller) GetAllDevices(req *Request) *Response {
	devs := c.hk.GetAllDevices()
	res := convertDeviceList(devs)
	return &Response{
		Id:      req.Id,
		Payload: res,
	}

}

func (c *MqttHkontroller) GetPairedDevices(req *Request) *Response {
	devs := c.hk.GetPairedDevices()
	res := convertDeviceList(devs)
	return &Response{
		Id:      req.Id,
		Payload: res,
	}
}

func (c *MqttHkontroller) GetVerifiedDevices(req *Request) *Response {
	devs := c.hk.GetVerifiedDevices()
	res := convertDeviceList(devs)
	return &Response{
		Id:      req.Id,
		Payload: res,
	}
}

func (c *MqttHkontroller) GetDeviceInfo(req *Request) *Response {
	jj, err := req.Payload.MarshalJSON()
	if err != nil {
		return responseError(req, err)
	}
	var getDeviceRequest GetDeviceInfoRequest
	err = json.Unmarshal(jj, &getDeviceRequest)
	if err != nil || getDeviceRequest.Device == nil {
		return responseError(req, errors.New("wrong get_device_info request payload. should be {device: string}"))
	}
	dd := c.hk.GetDevice(*getDeviceRequest.Device)
	if dd == nil {
		return responseError(req, errors.New("device not found"))
	}
	info := convertDevice(dd)
	return &Response{
		Id:      req.Id,
		Payload: info,
	}
}

func (c *MqttHkontroller) GetAccessories(req *Request) *Response {
	jj, err := req.Payload.MarshalJSON()
	if err != nil {
		return responseError(req, err)
	}
	var getAccsRequest GetAccessoriesRequest
	err = json.Unmarshal(jj, &getAccsRequest)
	if err != nil || getAccsRequest.Device == nil {
		return responseError(req, errors.New("wrong get_accessory_info request payload. should be {device: string, aid: int}"))
	}
	dd := c.hk.GetDevice(*getAccsRequest.Device)
	if dd == nil {
		return responseError(req, errors.New("device not found"))
	}
	err = dd.GetAccessories()
	if err != nil {
		return responseError(req, err)
	}
	return &Response{
		Id:      req.Id,
		Payload: dd.Accessories(),
	}
}
func (c *MqttHkontroller) GetAccessoryInfo(req *Request) *Response {
	jj, err := req.Payload.MarshalJSON()
	if err != nil {
		return responseError(req, err)
	}
	var getAccInfoRequest GetAccessoryInfoRequest
	err = json.Unmarshal(jj, &getAccInfoRequest)
	if err != nil || getAccInfoRequest.Aid == nil || getAccInfoRequest.Device == nil {
		return responseError(req, errors.New("wrong get_accessory_info request payload. should be {device: string, aid: int}"))
	}
	dd := c.hk.GetDevice(*getAccInfoRequest.Device)
	if dd == nil {
		return responseError(req, errors.New("device not found"))
	}
	err = dd.GetAccessories()
	if err != nil {
		return responseError(req, err)
	}
	for _, aa := range dd.Accessories() {
		if aa.Id == uint64(*getAccInfoRequest.Aid) {
			return &Response{
				Id:      req.Id,
				Payload: aa,
			}
		}
	}
	return responseError(req, errors.New("accessory not found"))
}

func (c *MqttHkontroller) GetCharacteristic(req *Request) *Response {
	jj, err := req.Payload.MarshalJSON()
	if err != nil {
		return responseError(req, err)
	}
	var getCharRequest GetCharacteristicRequest
	err = json.Unmarshal(jj, &getCharRequest)
	if err != nil || getCharRequest.Device == nil || getCharRequest.Aid == nil || getCharRequest.Iid == nil {
		return responseError(req,
			errors.New("wrong get_characteristic request payload. should be {device: string, aid: int, iid: int}"))
	}
	dd := c.hk.GetDevice(*getCharRequest.Device)
	if dd == nil {
		return responseError(req, errors.New("device not found"))
	}
	info, err := dd.GetCharacteristic(uint64(*getCharRequest.Aid), uint64(*getCharRequest.Iid))
	if err != nil {
		return responseError(req, err)
	}
	return &Response{
		Id:      req.Id,
		Payload: info,
	}
}

func (c *MqttHkontroller) PutCharacteristic(req *Request) *Response {
	jj, err := req.Payload.MarshalJSON()
	if err != nil {
		return responseError(req, err)
	}
	var putCharRequest PutCharacteristicRequest
	err = json.Unmarshal(jj, &putCharRequest)
	if err != nil || putCharRequest.Device == nil || putCharRequest.Aid == nil || putCharRequest.Value == nil {
		return responseError(req,
			errors.New("wrong put_characteristic payload. should be {device: string, aid: int, iid: int, value: any}"))
	}
	dd := c.hk.GetDevice(*putCharRequest.Device)
	if dd == nil {
		return responseError(req, errors.New("device not found"))
	}
	err = dd.PutCharacteristic(uint64(*putCharRequest.Aid), uint64(*putCharRequest.Iid), putCharRequest.Value)
	if err != nil {
		return responseError(req, err)
	}
	return &Response{
		Id:      req.Id,
		Payload: "success",
	}
}

func (c *MqttHkontroller) SubscribeToCharacteristic(req *Request) *Response {
	jj, err := req.Payload.MarshalJSON()
	if err != nil {
		return responseError(req, err)
	}
	var subCharRequest SubscribeToCharacteristicRequest
	err = json.Unmarshal(jj, &subCharRequest)
	if err != nil || subCharRequest.Device == nil || subCharRequest.Aid == nil || subCharRequest.Iid == nil {
		return responseError(req,
			errors.New("wrong subscribe_characteristic request payload. should be {device: string, aid: int, iid: int}"))
	}
	dd := c.hk.GetDevice(*subCharRequest.Device)
	if dd == nil {
		return responseError(req, errors.New("device not found"))
	}
	_, err = dd.SubscribeToEvents(uint64(*subCharRequest.Aid), uint64(*subCharRequest.Iid))
	if err != nil {
		return responseError(req, err)
	}
	return &Response{
		Id:      req.Id,
		Payload: "success",
	}
}

func responseError(req *Request, err error) *Response {
	res := Response{
		Id:    req.Id,
		Error: err.Error(),
	}
	return &res
}

func (c *MqttHkontroller) publishResponse(res *Response) error {
	jj, err := json.Marshal(res)
	if err != nil {
		return err
	}
	if token := c.mq.Publish(topicResponse, c.qos, false, jj); token.Wait() && token.Error() != nil {
		return token.Error()
	}
	return nil
}

func (c *MqttHkontroller) Subscribe() error {
	if token := c.mq.Subscribe(topicRequest, c.qos, func(mqttClient mqtt.Client, msg mqtt.Message) {
		var req Request
		err := json.Unmarshal(msg.Payload(), &req)
		if err != nil {
			fmt.Println("error unmarshalling message: ", err)
			return
		}

		var res *Response
		switch req.Action {
		case "pair":
			res = c.PairDevice(&req)
		case "verify":
			res = c.VerifyDevice(&req)
		case "unpair":
			res = c.UnpairDevice(&req)
		case "get_all_devices":
			res = c.GetAllDevices(&req)
		case "get_paired_devices":
			res = c.GetPairedDevices(&req)
		case "get_verified_devices":
			res = c.GetVerifiedDevices(&req)
		case "get_device_info":
			res = c.GetDeviceInfo(&req)
		case "get_accessories":
			res = c.GetAccessories(&req)
		case "get_accessory_info":
			res = c.GetAccessoryInfo(&req)
		case "get_characteristic":
			res = c.GetCharacteristic(&req)
		case "put_characteristic":
			res = c.PutCharacteristic(&req)
		case "subscribe_to_characteristic":
			res = c.SubscribeToCharacteristic(&req)
		default:
			res = &Response{
				Id:    req.Id,
				Error: "unknown action",
			}
		}
		c.publishResponse(res)
	}); token.Wait() && token.Error() != nil {
		return token.Error()
	}

	return nil
}

func main() {
	broker := flag.String("broker", "tcp://localhost:1883", "The broker URI. ex: tcp://10.10.1.1:1883")
	id := flag.String("id", "hkontroller", "The ClientID (optional)")
	hapId := flag.String("hap_id", "hkontroller", "The homekit client id")
	configDir := flag.String("config_dir", path.Join(os.Getenv("HOME"), ".config"), "Directory to store pairings")
	user := flag.String("user", "", "The User (optional)")
	password := flag.String("password", "", "The password (optional)")
	qos := flag.Int("qos", 0, "The Quality of Service 0,1,2 (default 0)")
	cleansess := flag.Bool("clean", false, "Set Clean Session (default false)")
	flag.Parse()

	opts := mqtt.NewClientOptions()
	opts.AddBroker(*broker)
	opts.SetClientID(*id)
	opts.SetUsername(*user)
	opts.SetPassword(*password)
	opts.SetCleanSession(*cleansess)

	pubsub := mqtt.NewClient(opts)
	if token := pubsub.Connect(); token.Wait() && token.Error() != nil {
		panic(token.Error())
	}
	store := hkontroller.NewFsStore(path.Join(*configDir, "hkontroller"))
	hk, err := hkontroller.NewController(store, *hapId)
	if err != nil {
		panic(err)
	}

	err = hk.LoadPairings()
	if err != nil {
		panic(err)
	}

	controller := &MqttHkontroller{
		mq:  pubsub,
		hk:  hk,
		qos: byte(*qos),
	}
	err = controller.Subscribe()
	if err != nil {
		panic(err)
	}
	controller.StartDiscovery()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	<-c

	pubsub.Disconnect(1000)
}
