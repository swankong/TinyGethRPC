package server

import (
    "fmt"
    "TinyGethRPC/rpc"
)


type PublicServerAPI struct{

}


// apis returns the collection of built-in RPC APIs.
func Apis() []rpc.API {
	return []rpc.API{
            {
		        Namespace: "tiny",
			    Version:   "1.0",
			    Service:   &PublicServerAPI{},
                Public:    true,
            },
    }
}

// Test RPC function
func (s *PublicServerAPI) HelloWorld(name string) string {
    res := fmt.Sprintf("%s, hello world!\n", name)
    return  res
}

