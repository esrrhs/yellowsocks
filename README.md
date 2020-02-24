# yellowsocks
yellowsocks类似于[redsocks](https://github.com/darkk/redsocks)，是依赖iptables把tcp转换为socks5的全局代理工具。

# 配置代理
在linux或者路由器上启动，指定监听端口、socks5的地址端口。
```
./yellowsocks -l :1234 -t 127.0.0.1:1080
```
# 配置iptables
```

*nat

:PREROUTING ACCEPT [0:0]

:INPUT ACCEPT [0:0]

:OUTPUT ACCEPT [0:0]

:POSTROUTING ACCEPT [0:0]

:REDSOCKS - [0:0]

# Redirect all output through redsocks

-A OUTPUT -p tcp -j REDSOCKS

# Whitelist LANs and some other reserved addresses.

# https://en.wikipedia.org/wiki/Reserved_IP_addresses#Reserved_IPv4_addresses

-A REDSOCKS -d 0.0.0.0/8 -j RETURN

-A REDSOCKS -d 10.0.0.0/8 -j RETURN

-A REDSOCKS -d 127.0.0.0/8 -j RETURN

-A REDSOCKS -d 169.254.0.0/16 -j RETURN

-A REDSOCKS -d 172.16.0.0/12 -j RETURN

-A REDSOCKS -d 192.168.0.0/16 -j RETURN

-A REDSOCKS -d 224.0.0.0/4 -j RETURN

-A REDSOCKS -d 240.0.0.0/4 -j RETURN

-A REDSOCKS -d {socks5的ip地址} -j RETURN

-A REDSOCKS -p tcp --dport {socks5的端口} -j RETURN

# Redirect everything else to redsocks port

-A REDSOCKS -p tcp -j REDIRECT --to-ports 31338

COMMIT

```
