# yellowsocks
yellowsocks类似于[redsocks](https://github.com/darkk/redsocks)，是依赖iptables把tcp转换为socks5的全局代理工具。

# 配置代理
在linux或者路由器上启动，指定监听端口、socks5的地址端口。
```
./yellowsocks -l :4455 -t 127.0.0.1:1080
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

-A REDSOCKS -p tcp -j REDIRECT --to-ports 4455

COMMIT

```

# openwrt配置
* 编译yellowsocks成路由器对应版本，如GOOS=linux GOARCH=mipsle go build
* 部分CPU需要在kernel里打开FPU，重新编译openwrt固件，编译方法参考官网
* 路由器启动后，在/etc/firewall.user添加转发规则
```
iptables -t nat -N YELLOWSOCKS
iptables -t nat -A PREROUTING -i br-lan -p tcp -j YELLOWSOCKS

# Do not redirect traffic to the followign address ranges
iptables -t nat -A YELLOWSOCKS -d 0.0.0.0/8 -j RETURN
iptables -t nat -A YELLOWSOCKS -d 10.0.0.0/8 -j RETURN
iptables -t nat -A YELLOWSOCKS -d 127.0.0.0/8 -j RETURN
iptables -t nat -A YELLOWSOCKS -d 169.254.0.0/16 -j RETURN
iptables -t nat -A YELLOWSOCKS -d 172.16.0.0/16 -j RETURN
iptables -t nat -A YELLOWSOCKS -d 192.168.0.0/16 -j RETURN
iptables -t nat -A YELLOWSOCKS -d 224.0.0.0/4 -j RETURN
iptables -t nat -A YELLOWSOCKS -d 240.0.0.0/4 -j RETURN

# Redirect all kinds of traffic
iptables -t nat -A YELLOWSOCKS -p tcp -j REDIRECT --to-ports 4455
```
* 启动yellowsocks
```
./yellowsocks -l :4455 -t sock5的ip:sock5的端口 -nolog 1 -noprint 1
```
* 如果上不去网，说明有dns污染，可以通过pingtunnel解决
* 关闭openwrt自带的dnsmasq的dns功能
```
uci -q delete dhcp.@dnsmasq[0].domain
uci set dhcp.@dnsmasq[0].port="0"
uci commit dhcp
/etc/init.d/dnsmasq restart
```
* 启动pingtunnel，转发dns到远端
```
./pingtunnel -type client -l :53 -s yourserver -t 8.8.8.8:53 -key yourkey -nolog 1 -noprint 1
```
