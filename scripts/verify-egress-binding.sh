#!/usr/bin/env sh
# 在 Linux 容器内验证「按 Key 绑定出口源 IP」确实生效。
#
# 为什么需要这个脚本:
#   P1-6 指出的失效模式是「静默」的 —— 未配置策略路由时，绑定源地址的请求
#   不会报错，而是照常成功，只是全部走了主 IP。单测无法在 macOS 宿主机上
#   复现（无权添加环回别名），因此在容器内添加真实别名地址来验证。
#
# 用法: docker run --rm --cap-add=NET_ADMIN -v "$PWD":/src -w /src golang:1.23-alpine sh scripts/verify-egress-binding.sh
set -e

echo "==> 添加环回别名地址"
ip addr add 127.0.0.2/8 dev lo 2>/dev/null || true
ip addr add 127.0.0.3/8 dev lo 2>/dev/null || true
ip addr show lo | grep "inet 127" || true

echo "==> 运行出口绑定验证"
export GOFLAGS=-mod=mod
export GOPROXY=${GOPROXY:-https://goproxy.cn,direct}
go test ./internal/egress/ -run 'TestLocalAddrBinding|TestVerify' -v -count=1
