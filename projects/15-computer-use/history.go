package cua

// 长循环里截图会越积越多：每张一千多 token；一个请求超过 20 张图后，每张图的尺寸上限变严。
// 删旧截图会改变前缀、打掉提示缓存。所以要“攒着一起删”：每隔 Every 轮删一次，只留最近 Keep 张。

type PrunePolicy struct {
	Keep  int // 每次清理后保留最近几张
	Every int // 每隔几轮清理一次
}

// Simulate 跑 turns 轮，每轮新增一张截图。返回：前缀被改动的次数（缓存失效次数）和单个请求里最多的图片数。
func (p PrunePolicy) Simulate(turns int) (prefixChanges, maxImages int) {
	images := 0
	for t := 1; t <= turns; t++ {
		images++
		if p.Every > 0 && t%p.Every == 0 && images > p.Keep {
			images = p.Keep
			prefixChanges++
		}
		if images > maxImages {
			maxImages = images
		}
	}
	return
}
