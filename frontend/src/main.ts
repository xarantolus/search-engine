import { createApp } from 'vue'
import Index from './pages/index.vue'
import App from './App.vue'
import { createRouter, createWebHistory } from 'vue-router'

const routes = [
	{ path: '/', component: Index },
	{ path: '/admin', component: () => import('./pages/admin.vue') },
]

const router = createRouter({
	history: createWebHistory(),
	routes
})

createApp(App)
	.use(router)
	.mount('#app')
