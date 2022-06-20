#!/bin/bash
protoc -I . lspd.proto --go-grpc_out=./
