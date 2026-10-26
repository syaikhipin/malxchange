<template>
  <NuxtLayout>
    <!-- :page-key remounts param routes when the param changes, so
         pages like /exchange/:pair resolve the new market fresh on
         mount instead of silently keeping the old one's data (audit
         issue #97). -->
    <NuxtPage :page-key="$route.fullPath" />
  </NuxtLayout>
</template>

<script setup lang="ts">
// Every page sets its own title through useHead; this template appends
// the site name so no page can ship an ambiguous document title.
useHead({
  titleTemplate: (title) => (title ? `${title} · malxchange` : 'malxchange'),
})
</script>
