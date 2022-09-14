package server

type ServerConfig struct {
    HttpCors []string
    HttpVirtualHosts []string
    HttpModules []string
    HttpPathPrefix string
    HttpHost string
}

var (
    Config = ServerConfig {
        HttpCors: []string{"*"},
        HttpVirtualHosts: []string{"*"},
        HttpModules: []string{"tiny"},
        HttpPathPrefix: "",
        HttpHost: "0.0.0.0",
    }
)
