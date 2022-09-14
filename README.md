# TinyGethRPC
A light weighted RPC framwork extraced from Geth (Go-Ethereum)

一个基于Geth实现的轻量级RPC框架
1. 所有代码从Geth实现中抽取出来
2. 下载代码，运行go build 编译，运行 TinyGethRPC执行，默认服务端口9988
3. 使用者可以编辑service/api.go添加自己的处理逻辑，增加更多RPC服务方法
4. 示例展示。编译后，运行 ./TinyGethRPC 
测试：curl -XPOST http://hostname:9988 -H 'content-type: application/json' -d'{"method": "tiny_helloWorld", "params":["Zhangsan"], id:1}'
预期返回结果：{"jsonrpc":"2.0","id":1,"result":"Zhangsan, hello world!\n"}
