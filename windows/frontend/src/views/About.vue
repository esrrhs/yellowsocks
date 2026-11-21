<script lang="ts" setup>
import { onMounted, ref } from "vue";
import { GetVersion } from "../../wailsjs/go/guiapp/App";
import { BrowserOpenURL } from "../../wailsjs/runtime/runtime";
import { version } from "../../wailsjs/go/models";

const info = ref<version.Info | null>(null);

onMounted(async () => {
  info.value = new version.Info(await GetVersion());
});
</script>

<template>
  <section class="page">
    <h2>关于</h2>
    <div class="card about-card">
      <div class="logo big">YS</div>
      <div class="app-name">YellowSocks</div>
      <table v-if="info" class="info-table">
        <tbody>
          <tr>
            <td>版本</td>
            <td>{{ info.version }}</td>
          </tr>
          <tr>
            <td>提交</td>
            <td>{{ info.git_commit }}</td>
          </tr>
          <tr>
            <td>构建时间</td>
            <td>{{ info.build_time }}</td>
          </tr>
          <tr>
            <td>运行环境</td>
            <td>{{ info.os }}/{{ info.arch }} · Go {{ info.go_version }}</td>
          </tr>
        </tbody>
      </table>
      <button class="btn" @click="BrowserOpenURL('https://github.com/esrrhs/yellowsocks')">
        GitHub 项目主页
      </button>
    </div>
  </section>
</template>
