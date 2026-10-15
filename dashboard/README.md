# dashboard

中继运行面板。**一个 HTML 文件，无构建、无依赖、无外部请求** —— 没有 CDN、
没有 Web Font、没有图表库，所以它既能被二进制直接吐出来，也能单独部署。

![运行面板 · 浅色](screenshot-light.png)

深色主题：

![运行面板 · 深色](screenshot-dark.png)

## 三种用法

1. **跟服务一起用**：`api_dashboard: true`（命令行默认开）时，
   `http://<api_listen>/` 就是这个页面。

2. **单独部署**（页面在 A 机、API 在 B 机）：把 `index.html` 拷到任意静态托管上

   ```bash
   curl -sL https://raw.githubusercontent.com/esrrhs/yellowsocks/master/dashboard/index.html \
     -o /var/www/yellowsocks/index.html
   ```

   打开后点右上「设置」，填 B 机地址和 token。此时需要 B 机开 `api_origins`
   允许这个来源跨域。

3. **本地直接打开**：双击文件，填 API 地址即可（file:// 下同样能填）。

## 让 token 不进页面源码

把页面直接指向 B 机、token 存在浏览器里，任何人拿到页面源码 + 浏览器
localStorage 就能读走。更稳的做法是页面只写相对路径，由 A 机上的 nginx
反代 `/api/` 到 B 并在服务端注入 token，见根目录 README
「Deploying the page away from the relay」。

## URL 参数

```
?server=http://10.0.0.2:9090   # API 地址，留空=同源
&token=xxx                     # 自动存进 localStorage
&theme=light|dark|auto         # 主题
```

## 交互速查

- 顶部 `◐` 切浅色/深色，设置里可切「跟随系统」。
- 流量图：图例可单独关掉上传/下载；`30分/1时/6时/24时` 切时间范围；鼠标悬停有十字线读数。
- 活跃连接：点表头按协议/路由/时长/上下行排序，右侧输入框按域名或客户端过滤。
- 设置 → 「导出快照」下载一份完整 JSON；「重置统计」需要管理员 token。
