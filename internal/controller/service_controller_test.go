package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("Service Controller", func() {

	// 테스트에 사용할 상수 및 변수 정의
	const (
		TestLBNamespace = "default"
		// controller/service_controller.go 에 정의된 상수와 동일한 값을 사용해야 합니다.
		TargetNamespace      = "external-targets"
		ManagedAnnotationKey = "linker.hsj.com/managed"
		ManagedAnnotationVal = "true"
		LBFinalizer          = "lb-linker/finalizer"
	)

	var (
		ctx        context.Context
		reconciler *ServiceReconciler // reconciler 변수를 여기에 선언
		cancel     context.CancelFunc
	)

	// 각 테스트("It" 블록)가 실행되기 전에 필요한 환경을 설정합니다.
	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.Background())

		// 테스트용 매니저 생성
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme: scheme.Scheme,
		})

		Expect(err).NotTo(HaveOccurred())
		reconciler = &ServiceReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
			Log:    ctrl.Log.WithName("tests").WithName("ServiceController"),
		}

		// 매니저에 Reconciler등록
		err = reconciler.SetupWithManager(mgr)
		Expect(err).NotTo(HaveOccurred())

		// 테스트를 위해 매니저를 별도의 goroutine에서 실행
		go func() {
			defer GinkgoRecover()
			err := mgr.Start(ctx)
			Expect(err).NotTo(HaveOccurred(), "Failed to run manager")
		}()

		// TargetNamespace가 없으면 생성
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: TargetNamespace}}
		// `Create`는 이미 존재하면 에러를 반환하므로, `Get`으로 확인 후 없으면 생성합니다.
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: TargetNamespace}, &corev1.Namespace{}); err != nil {
			if apierrors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, ns)).Should(Succeed())
			}
		}
	})

	// 각 테스트가 끝난 후 생성했던 리소스들을 정리합니다.
	AfterEach(func() {
		// 테스트 중에 생성된 모든 Service를 정리합니다.
		By("cleaning up resources after test")
		cancel()

		cleanupCtx := context.Background()
		Expect(k8sClient.DeleteAllOf(cleanupCtx, &corev1.Service{}, client.InNamespace(TestLBNamespace))).Should(Succeed())
		Expect(k8sClient.DeleteAllOf(cleanupCtx, &corev1.Service{}, client.InNamespace(TargetNamespace))).Should(Succeed())

		// 리소스 정리가 끝난 후, 매니저 실행 컨텍스트를 취소하여 매니저를 중지시킵니다.
	})

	// === 테스트 케이스 시작 ===

	Context("when a managed LoadBalancer service is created", func() {
		It("should create an ExternalName service and add a finalizer", func() {
			By("1. creating a new LoadBalancer service with the managed annotation")
			lbName := "test-create-lb"
			lbServiceKey := types.NamespacedName{Name: lbName, Namespace: TestLBNamespace}
			lbService := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      lbName,
					Namespace: TestLBNamespace,
					Annotations: map[string]string{
						ManagedAnnotationKey: ManagedAnnotationVal,
					},
				},
				Spec: corev1.ServiceSpec{
					Type:  corev1.ServiceTypeLoadBalancer,
					Ports: []corev1.ServicePort{{Port: 80}},
				},
			}
			Expect(k8sClient.Create(ctx, lbService)).Should(Succeed())

			By("2. updating the LoadBalancer status with an external hostname")
			// Status는 실제 컨트롤러가 없으므로 테스트에서 수동으로 업데이트 해줘야 합니다.
			Eventually(func() error {
				err := k8sClient.Get(ctx, lbServiceKey, lbService)
				if err != nil {
					return err
				}
				lbService.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
					{Hostname: "created-lb.example.com"},
				}
				return k8sClient.Status().Update(ctx, lbService)
			}, time.Second*10, time.Millisecond*250).Should(Succeed())

			By("3. verifying the ExternalName service is created and the finalizer is added")
			// Reconcile 로직에 따라 예상되는 ExternalName 서비스의 이름
			expectedExtName := reconciler.sanitizeName(fmt.Sprintf("%s-%s-ext", lbName, TestLBNamespace))
			extNameSvcKey := types.NamespacedName{Name: expectedExtName, Namespace: TargetNamespace}

			// 오퍼레이터가 비동기적으로 작업을 완료할 때까지 기다리며 검증합니다.
			Eventually(func(g Gomega) {
				// LB에 Finalizer가 추가되었는지 확인
				updatedLBService := &corev1.Service{}
				err := k8sClient.Get(ctx, lbServiceKey, updatedLBService)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(updatedLBService.Finalizers).To(ContainElement(LBFinalizer))

				// ExternalName 서비스가 생성되었고 Target이 올바른지 확인
				createdExtNameSvc := &corev1.Service{}
				err = k8sClient.Get(ctx, extNameSvcKey, createdExtNameSvc)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(createdExtNameSvc.Spec.ExternalName).To(Equal("created-lb.example.com"))
			}, time.Second*10, time.Millisecond*250).Should(Succeed())
		})
	})

	Context("when a managed LoadBalancer service is deleted", func() {
		It("should delete the corresponding ExternalName Service and remove the finalizer", func() {
			By("1. creating a managed LB and waiting for its ExternalName to be ready")
			lbName := "test-delete-lb"
			lbServiceKey := types.NamespacedName{Name: lbName, Namespace: TestLBNamespace}
			lbService := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      lbName,
					Namespace: TestLBNamespace,
					Annotations: map[string]string{
						ManagedAnnotationKey: ManagedAnnotationVal,
					},
				},
				Spec: corev1.ServiceSpec{
					Type:  corev1.ServiceTypeLoadBalancer,
					Ports: []corev1.ServicePort{{Port: 80}},
				},
			}
			Expect(k8sClient.Create(ctx, lbService)).Should(Succeed())

			// Status 업데이트
			Eventually(func() error {
				err := k8sClient.Get(ctx, lbServiceKey, lbService)
				if err != nil {
					return err
				}
				lbService.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
					{Hostname: "deleted-lb.example.com"},
				}
				return k8sClient.Status().Update(ctx, lbService)
			}, time.Second*10, time.Millisecond*250).Should(Succeed())

			// ExternalName 생성 및 Finalizer 추가 확인
			expectedExtName := reconciler.sanitizeName(fmt.Sprintf("%s-%s-ext", lbName, TestLBNamespace))
			extNameSvcKey := types.NamespacedName{Name: expectedExtName, Namespace: TargetNamespace}
			Eventually(func(g Gomega) {
				updatedLBService := &corev1.Service{}
				err := k8sClient.Get(ctx, lbServiceKey, updatedLBService)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(updatedLBService.Finalizers).To(ContainElement(LBFinalizer))
			}, time.Second*10, time.Millisecond*250).Should(Succeed())

			By("2. deleting the source LoadBalancer service")
			Expect(k8sClient.Delete(ctx, lbService)).Should(Succeed())

			By("3. verifying the ExternalName is deleted and the LoadBalancer is finally gone")
			Eventually(func(g Gomega) {
				// ExternalName 서비스가 삭제되었는지 확인 (Not Found 에러 기대)
				err := k8sClient.Get(ctx, extNameSvcKey, &corev1.Service{})
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())

				// LoadBalancer 서비스가 최종적으로 삭제되었는지 확인 (Not Found 에러 기대)
				err = k8sClient.Get(ctx, types.NamespacedName{Name: lbName, Namespace: TestLBNamespace}, &corev1.Service{})
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}, time.Second*10, time.Millisecond*250).Should(Succeed())
		})
	})

	Context("when an unmanaged LoadBalancer service is created", func() {
		It("should not create an ExternalName service and not add a finalizer", func() {
			By("1. creating a new LoadBalancer service without the managed annotation")
			lbName := "test-unmanaged-lb"
			lbServiceKey := types.NamespacedName{Name: lbName, Namespace: TestLBNamespace}
			lbService := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      lbName,
					Namespace: TestLBNamespace,
					// 어노테이션 없음!
				},
				Spec: corev1.ServiceSpec{
					Type:  corev1.ServiceTypeLoadBalancer,
					Ports: []corev1.ServicePort{{Port: 80}},
				},
			}
			Expect(k8sClient.Create(ctx, lbService)).Should(Succeed())

			By("2. verifying that no ExternalName service is created and no finalizer is added")
			expectedExtName := reconciler.sanitizeName(fmt.Sprintf("%s-%s-ext", lbName, TestLBNamespace))
			extNameSvcKey := types.NamespacedName{Name: expectedExtName, Namespace: TargetNamespace}

			// Consistently는 Eventually와 반대로, 일정 시간 동안 상태가 계속 유지되는지 확인합니다.
			// 즉, ExternalName이 생성되지 않고, Finalizer도 추가되지 않는 상태가 유지되어야 합니다.
			Consistently(func(g Gomega) {
				// LB에 Finalizer가 없는지 확인
				updatedLBService := &corev1.Service{}
				err := k8sClient.Get(ctx, lbServiceKey, updatedLBService)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(updatedLBService.Finalizers).To(BeEmpty())

				// ExternalName 서비스가 없는지 확인
				err = k8sClient.Get(ctx, extNameSvcKey, &corev1.Service{})
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}, time.Second*3, time.Millisecond*250).Should(Succeed())
		})
	})
})
