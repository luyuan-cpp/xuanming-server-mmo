// 《五行奇谈》游戏外独立开发者支持页配置文件（100% 游戏客户端之外独立部署）。
// 使用方式：将下方对应的链接替换成你自己在各平台开通的真实主页地址即可。
// 安全保护：任何留空 ("") 的渠道或档位会自动隐藏，页面绝不出现失效死链。
// 红线声明：本页严禁嵌入或链接进任何游戏客户端（Unity / 小游戏 / App），且不兑换任何游戏内道具/货币/特权。

window.EXTERNAL_SUPPORT_CONFIG = {
  brand: {
    gameTitle: "五行奇谈",
    subtitle: "个人独立研发回合制 MMORPG · 开发日志与社区自愿赞助页",
    developerName: "独立开发者",
    tagline: "不肝、不逼氪、不卖数值——你的每一次关注、建议与自愿赞助，都会用来支撑日常服务器运维、美术素材与持续更新。",
    updatedAt: "2026-11-26"
  },

  // 支持渠道（留空自动隐藏；建议国内优先开通「爱发电」，海外优先开通「Ko-fi / GitHub Sponsors」）
  channels: [
    {
      id: "afdian",
      region: "国内首选 · 微信 / 支付宝",
      name: "爱发电（Afdian）",
      badge: "推荐",
      description: "支持单次请喝茶或按月发电，平台统一合规结算与提现，零个人收款码风控，适合国内玩家支持开发日志更新。",
      buttonText: "前往爱发电主页",
      url: "", // 示例: "https://afdian.com/a/你的爱发电ID"
      highlight: true
    },
    {
      id: "kofi",
      region: "海外 · PayPal / 信用卡",
      name: "Ko-fi",
      badge: "0% 平台抽成",
      description: "海外玩家一键请作者喝杯茶，支持单次小额赞助与月度会员，无平台固定抽成。",
      buttonText: "在 Ko-fi 支持",
      url: "", // 示例: "https://ko-fi.com/你的账号"
      highlight: false
    },
    {
      id: "github",
      region: "开源技术赞助",
      name: "GitHub Sponsors",
      badge: "开源引擎",
      description: "支持服务端架构、双版本（C++/Go + Java）框架与自动化工具链的持续开源维护。",
      buttonText: "前往 GitHub Sponsors",
      url: "", // 示例: "https://github.com/sponsors/你的GitHubID"
      highlight: false
    },
    {
      id: "patreon",
      region: "海外月度会员",
      name: "Patreon",
      badge: "开发手记",
      description: "海外按月订阅开发者周报、架构复盘与幕后创作随笔。",
      buttonText: "前往 Patreon",
      url: "", // 示例: "https://www.patreon.com/你的账号"
      highlight: false
    }
  ],

  // 推荐心意档位说明（仅用于透明展示用途，实际支付全部跳转上方正规第三方平台完成，本站不经手任何资金）
  tiers: [
    {
      price: "¥6",
      title: "一杯清茶",
      purpose: "给熬夜修 Bug 的开发者提神，纯心意鼓励。",
      perk: "开发日志公开鸣谢（自愿登记昵称）"
    },
    {
      price: "¥18",
      title: "服务器续命包",
      purpose: "分担测试服 / 官网带宽与云主机日常成本。",
      perk: "开发日志公开鸣谢 + 社区抢先看研发周报"
    },
    {
      price: "¥30+",
      title: "长期同行赞助",
      purpose: "支持新玩法原型、原创美术素材与长期维护。",
      perk: "开发日志特别鸣谢 + 新玩法社区投票参与"
    }
  ],

  // 官方开发日志与玩家社区入口（留空自动隐藏）
  communityLinks: [
    { name: "TapTap 开发者主页", url: "" },
    { name: "B站开发日志", url: "" },
    { name: "GitHub 开源仓库", url: "" },
    { name: "联系邮箱", url: "" } // 示例: "mailto:dev@yourdomain.com"
  ]
};
