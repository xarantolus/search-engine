<script setup lang="ts">
import { onMounted, ref } from 'vue'

const loading = ref(false)
const error = ref('')
const state = ref<any>(null)

const checkedPermissions = ref<{ [userKey: string]: { [permName: string]: boolean } }>({})

function initPermissions() {
	if (!state.value) return
	state.value.users.forEach((user: any) => {
		checkedPermissions.value[user.UserKey] = {}
		state.value.permissions.forEach((perm: any) => {
			checkedPermissions.value[user.UserKey][perm.Name] = user.PermissionGroups?.includes(perm.Name)
		})
	})
}

async function onPermissionChange(userId: string) {
	// Gather all checked names for the user
	const newGroups = Object.entries(checkedPermissions.value[userId])
		.filter(([_, isChecked]) => isChecked)
		.map(([groupName]) => groupName)

	console.log('User', userId, 'has permission groups:', newGroups)

	// Update the user's permission groups
	try {
		const res = await fetch("/api/v1/admin/permissions", {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({
				user_id: userId,
				permission_groups: newGroups
			 })
		});
		if (!res.ok) {
			// If json, try to parse and show the error message
			const json = await res.json()
			if (json && json.error) {
				throw new Error(json.error)
			}
			throw new Error('Failed to update permissions')
		}
	} catch (err) {
		alert(`Failed to update permissions: ${err}`)
	}
}

async function loadData() {
	const response = await fetch("/api/v1/admin/permissions", {
		method: 'GET',
		headers: { 'Content-Type': 'application/json' }
	})
	if (!response.ok) {
		if (response.status === 401 || response.status === 403) {
			location.href = '/login?redirect=' + encodeURIComponent(location.pathname + location.search)
		}
		throw new Error('Network response was not ok')
	}
	return await response.json()
}

onMounted(async () => {
	try {
		loading.value = true
		state.value = await loadData()
		initPermissions()
	} catch (err: any) {
		error.value = err.message
	} finally {
		loading.value = false
	}
})

const deleteDocumentID = ref('')
const deletionLoading = ref(false)
const deletionMessage = ref('')

async function deleteDocument() {
	if (!deleteDocumentID.value) {
		alert('Please enter a document ID to delete.')
		return
	}

	deletionLoading.value = true
	try {
		const response = await fetch(`/api/v1/doc/${deleteDocumentID.value}`, {
			method: 'DELETE',
			headers: { 'Content-Type': 'application/json' }
		})
		if (!response.ok) {
			const json = await response.json()
			throw new Error(json.error || 'Failed to delete document')
		}
		deletionMessage.value = `Document ${deleteDocumentID.value} deleted successfully.`
		deleteDocumentID.value = ''
	} catch (err: any) {
		alert(`Error deleting document: ${err.message}`)
	} finally {
		deletionLoading.value = false
	}
}
</script>

<template>
	<div class="container">
		<h1>User Management</h1>
		<p v-if="loading">Loading...</p>
		<template v-else>
			<p style="color: red;" v-if="error">{{ error }}</p>
			<div v-if="state">
				<table>
					<thead>
						<tr>
							<th>User</th>
							<th v-for="perm in state.permissions" :key="perm.Name">
								{{ perm.Name }}
							</th>
						</tr>
					</thead>
					<tbody>
						<tr v-for="user in state.users" :key="user.UserKey">
							<td>{{ user.DisplayName }}</td>
							<td v-for="perm in state.permissions" :key="perm.Name">
								<input type="checkbox" v-model="checkedPermissions[user.UserKey][perm.Name]" @change="onPermissionChange(user.UserKey)" />
							</td>
						</tr>
					</tbody>
				</table>
			</div>
		</template>

		<h2>Delete Document</h2>
		<input type="text" v-model="deleteDocumentID" placeholder="Document ID" :disabled="deletionLoading" />
		<button @click="deleteDocument" :disabled="deletionLoading">Delete Document</button>
		<p v-if="deletionLoading">Deleting...</p>
		<p v-if="deletionMessage">{{ deletionMessage }}</p>
		<p v-if="!deletionMessage && !deletionLoading">Enter a document ID to delete.</p>
	</div>
</template>

<style scoped>
.container {
	margin: 20px;
}

table,
th,
td {
	border: 1px solid #ccc;
	border-collapse: collapse;
	padding: 5px;
	text-align: left;
}
</style>
