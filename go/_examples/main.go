package main

import (
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	gate "github.com/gateio/gatews/go"
)

// 本示例演示如何订阅 Gate.io WebSocket 频道
// 特别关注 stp_id 字段的解析验证（现已修复为 string 类型）
//
// 验证要点：
// 1. 订阅 spot.orders 和 futures.orders 频道
// 2. 接收订单更新消息
// 3. 验证 stp_id 字段能正确解析为 string 类型
// 4. 确认不再出现 "cannot unmarshal string into Go struct field" 错误
//
// 使用前请替换为您的真实 API Key 和 Secret

func main() {
	// create WsService with ConnConf, this is recommended, key and secret will be needed by some channels
	// ctx and logger could be nil, they'll be initialized by default
	ws, err := gate.NewWsService(nil, nil, gate.NewConnConfFromOption(&gate.ConfOptions{
		Key:           "7132da9e7225c7b91370eeb7e8b195d4",
		Secret:        "789aac107a968d5cbbd5aa8855cc2d427ce824846342a02b36d18c785b67d3e7",
		MaxRetryConn:  10, // default value is math.MaxInt64, set it when needs
		SkipTlsVerify: false,
	}))
	// we can also do nothing to get a WsService, all parameters will be initialized by default and default url is spot
	// but some channels need key and secret for auth, we can also use set function to set key and secret
	// ws, err := gate.NewWsService(nil, nil, nil)
	// ws.SetKey("YOUR_API_KEY")
	// ws.SetSecret("YOUR_API_SECRET")
	if err != nil {
		log.Printf("NewWsService err:%s", err.Error())
		return
	}

	// checkout connection status when needs
	go func() {
		ticker := time.NewTicker(time.Second)
		for {
			<-ticker.C
			log.Println("connetion status:", ws.Status())
		}
	}()

	// create callback functions for receive messages
	callOrder := gate.NewCallBack(func(msg *gate.UpdateMsg) {
		if msg.Event != "update" {
			return
		}
		log.Printf("收到现货订单消息，原始数据: %s", string(msg.Result))

		// parse the message to struct we need
		var orders []gate.SpotOrderMsg
		if err := json.Unmarshal(msg.Result, &orders); err != nil {
			log.Printf("现货订单解析错误: %v", err)
			return
		}

		for _, order := range orders {
			log.Printf("现货订单详情:")
			log.Printf("  订单ID: %s", order.Id)
			log.Printf("  交易对: %s", order.CurrencyPair)
			log.Printf("  方向: %s", order.Side)
			log.Printf("  数量: %s", order.Amount)
			log.Printf("  价格: %s", order.Price)
			log.Printf("  ✓ STP ID: '%s' (类型: string)", order.StpId)
			log.Printf("  ✓ STP Act: '%s'", order.StpAct)
			if order.StpId != "" && order.StpId != "0" {
				log.Printf("  ✓✓✓ 成功接收到非零 STP ID 值！")
			}
		}
	})

	// callback for futures orders - 验证 stp_id 字段
	callFuturesOrder := gate.NewCallBack(func(msg *gate.UpdateMsg) {
		if msg.Event != "update" {
			return
		}
		log.Printf("收到期货订单消息，原始数据: %s", string(msg.Result))

		var orders []gate.FuturesOrder
		if err := json.Unmarshal(msg.Result, &orders); err != nil {
			log.Printf("期货订单解析错误: %v", err)
			return
		}

		for _, order := range orders {
			log.Printf("期货订单详情:")
			log.Printf("  订单ID: %d", order.Id)
			log.Printf("  合约: %s", order.Contract)
			log.Printf("  数量: %d", order.Size)
			log.Printf("  价格: %f", order.Price)
			log.Printf("  状态: %s", order.FinishAs)
			log.Printf("  ✓ STP ID: '%s' (类型: string)", order.StpId)
			log.Printf("  ✓ STP Act: '%s'", order.StpAct)
			if order.StpId != "" && order.StpId != "0" {
				log.Printf("  ✓✓✓ 成功接收到非零 STP ID 值！")
			}
			log.Printf("  业务信息: %s", order.BizInfo)
		}
	})

	callTrade := gate.NewCallBack(func(msg *gate.UpdateMsg) {
		var trade gate.SpotTradeMsg
		if err := json.Unmarshal(msg.Result, &trade); err != nil {
			log.Printf("trade %s unmarshal err: %v", msg.Result, err)
		}
		log.Printf("trade: %+v", trade)
	})

	// first, we need set callback function
	ws.SetCallBack(gate.ChannelSpotOrder, callOrder)
	ws.SetCallBack(gate.ChannelFutureOrder, callFuturesOrder)
	ws.SetCallBack(gate.ChannelSpotPublicTrade, callTrade)

	// second, after set callback function, subscribe to any channel you are interested into
	log.Println("开始订阅频道...")

	// 订阅现货订单（需要 API key 认证）
	if err := ws.Subscribe(gate.ChannelSpotOrder, []string{"BTC_USDT"}); err != nil {
		log.Printf("订阅现货订单频道失败: %s", err.Error())
		return
	}
	log.Println("✓ 已订阅现货订单频道 (spot.orders)")

	// 订阅期货订单（需要 API key 认证）- 重点验证 stp_id 字段
	if err := ws.Subscribe(gate.ChannelFutureOrder, []string{"BTC_USDT"}); err != nil {
		log.Printf("订阅期货订单频道失败: %s", err.Error())
		return
	}
	log.Println("✓ 已订阅期货订单频道 (futures.orders) - 用于验证 stp_id 字段")

	// 订阅现货公共成交
	if err := ws.Subscribe(gate.ChannelSpotPublicTrade, []string{"BTC_USDT"}); err != nil {
		log.Printf("订阅现货成交频道失败: %s", err.Error())
		return
	}
	log.Println("✓ 已订阅现货成交频道 (spot.trades)")

	// example for maintaining local order book
	// LocalOrderBook(context.Background(), ws, []string{"BTC_USDT"})

	ch := make(chan os.Signal)
	signal.Ignore(syscall.SIGPIPE, syscall.SIGALRM)
	signal.Notify(ch, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT, syscall.SIGABRT, syscall.SIGKILL)
	<-ch
}
