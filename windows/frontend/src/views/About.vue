<script lang="ts" setup>
import { onMounted, ref } from "vue";
import { GetVersion } from "../../wailsjs/go/guiapp/App";
import { BrowserOpenURL } from "../../wailsjs/runtime/runtime";
import { version } from "../../wailsjs/go/models";
import { t } from "../i18n";

const info = ref<version.Info | null>(null);

onMounted(async () => {
  info.value = new version.Info(await GetVersion());
});
</script>

<template>
  <section class="page">
    <h2 class="about-title">{{ t("about.title") }}</h2>
    <div class="card about-card">
      <div class="logo big">YS</div>
      <div class="app-name">YellowSocks</div>
      <table v-if="info" class="info-table">
        <tbody>
          <tr>
            <td>{{ t("about.version") }}</td>
            <td>{{ info.version }}</td>
          </tr>
          <tr>
            <td>{{ t("about.commit") }}</td>
            <td>{{ info.git_commit }}</td>
          </tr>
          <tr>
            <td>{{ t("about.buildTime") }}</td>
            <td>{{ info.build_time }}</td>
          </tr>
          <tr>
            <td>{{ t("about.env") }}</td>
            <td>{{ info.os }}/{{ info.arch }} · {{ info.go_version }}</td>
          </tr>
        </tbody>
      </table>
      <button class="btn" @click="BrowserOpenURL('https://github.com/esrrhs/yellowsocks')">
        {{ t("about.github") }}
      </button>
    </div>
  </section>
</template>
