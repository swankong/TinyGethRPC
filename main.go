package main 


import (
    "log"
    "time"
    "TinyGethRPC/server"
)

const DEFAULT_PORT = 9988

func main() {
    serverPort := DEFAULT_PORT

    logger := log.Default()
    //server.Init(progConfig.Database)
    httpRPC := server.NewHttpServer(logger, server.DefaultHTTPTimeouts)
    if err := httpRPC.StartHttpRPC(server.Apis(), serverPort); err != nil {
        log.Fatal("Failed to start http rpc server")
    }

    for {
        time.Sleep(10 * time.Second)
    }

}

