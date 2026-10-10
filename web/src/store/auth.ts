import { create } from 'zustand';
import api from '@/api/client';
interface AuthState {
 isAuthenticated: boolean;
 login: (token: string) => void;
 logout: () => void;
 init: () => void;
}
export const useAuthStore = create<AuthState>((set) => ({
 isAuthenticated: false,
 login: (token) => { api.setToken(token); localStorage.setItem('brodge_token', token); localStorage.removeItem('cc_token'); set({isAuthenticated:true}); },
 logout: () => { api.setToken(''); localStorage.removeItem('cc_token'); localStorage.removeItem('brodge_token'); set({isAuthenticated:false}); },
 init: () => { const token=localStorage.getItem('brodge_token') || localStorage.getItem('cc_token'); if(token){ api.setToken(token); set({isAuthenticated:true}); } },
}));
