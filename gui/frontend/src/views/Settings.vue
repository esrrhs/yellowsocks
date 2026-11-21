<script lang="ts" setup>
import { onMounted, ref } from "vue";
import { GetConfig, SaveConfig } from "../../wailsjs/go/main/App";

const content = ref("");
const message = ref("");
const error = ref("");

async function load() {
  error.value = "";
  message.value = "";
  try {
    content.value = await GetConfig();
  } catch (e: any) {
    error.value = String(e?.message ?? e);
  }
}

async function save() {
  error.value = "";
  message.value = "";
  try {
    await SaveConfig(content.value);
    message.value = "已保存,启动或重启引擎后生效";
  } catch (e: any) {
    error.value = String(e?.message ?? e);
  }
}

onMounted(load);
</script>

<template>
  <section class="page">
    <h2>设置</h2>
    <p class="hint">
      配置为 YAML 格式,与 CLI 的 config.yaml 完全兼容,可互相拷贝使用。
      保存时会先做格式校验。全部字段见仓库根目录的 config.example.yaml。
    </p>
    <textarea
      v-model="content"
      class="editor"
      spellcheck="false"
      aria-label="配置内容"
    ></textarea>
    <div class="actions">
      <button class="btn primary" :disabled="!content" @click="save">保存</button>
      <button class="btn" @click="load">重新加载</button>
      <span v-if="message" class="msg ok">{{ message }}</span>
      <span v-if="error" class="msg err">{{ error }}</span>
    </div>
  </section>
</template>
