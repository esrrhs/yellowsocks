import {defineConfig} from 'vite'
import vue from '@vitejs/plugin-vue'

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [vue()],
  // src/i18n.ts imports the shared translation catalogue from
  // ../../../guiapp/locales: the window and the system tray render from the
  // same files. The dev server refuses to serve outside its root by default,
  // so the repository root has to be allowed explicitly; the production build
  // inlines the JSON and needs nothing.
  server: {
    fs: {
      allow: ['..']
    }
  }
})
